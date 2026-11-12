package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
	"github.com/eirueimi/unified-cd/internal/gittoken"
	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/eirueimi/unified-cd/internal/store"
)

// GitCredentialResolver turns stored GitCredentials into plaintext git
// credentials for the controller's own fetches (AppSource sync, uses:
// templates) and issues GitHub App job tokens. One resolver shares one
// gittoken.Issuer, so every user of it shares one token cache.
//
// A nil *GitCredentialResolver resolves every request to an empty
// (anonymous) Credential.
type GitCredentialResolver struct {
	st      store.Store
	km      secrets.KeyManager
	issuer  *gittoken.Issuer
	metrics *metrics.Metrics
}

// NewGitCredentialResolver returns a resolver. A nil issuer means a default
// gittoken.NewIssuer(nil, nil); a nil m records no metrics.
func NewGitCredentialResolver(st store.Store, km secrets.KeyManager, issuer *gittoken.Issuer, m *metrics.Metrics) *GitCredentialResolver {
	if issuer == nil {
		issuer = gittoken.NewIssuer(nil, nil)
	}
	return &GitCredentialResolver{st: st, km: km, issuer: issuer, metrics: m}
}

// SetGitCredentialResolver sets the resolver used to issue GitHub App job tokens.
func (s *Server) SetGitCredentialResolver(g *GitCredentialResolver) { s.gitCreds = g }

// controllerContentsRead is the permission set of every controller-side
// GitHub App token: read one repository's contents.
func controllerContentsRead() map[string]string { return map[string]string{"contents": "read"} }

// ForAppSource returns the credential for syncing an AppSource from repoURL.
// jobAccess does not apply: an administrator configured the AppSource. A
// githubApp credential needs an https repoURL.
func (g *GitCredentialResolver) ForAppSource(ctx context.Context, repoURL string) (gittemplate.Credential, error) {
	if g == nil {
		return gittemplate.Credential{}, nil
	}
	host := repoURLHost(repoURL)
	if host == "" {
		return gittemplate.Credential{}, nil
	}
	gc, err := g.st.GetGitCredentialByHost(ctx, host)
	if err != nil {
		return gittemplate.Credential{}, fmt.Errorf("get git credential for host %q: %w", host, err)
	}
	if gc == nil {
		return gittemplate.Credential{}, nil
	}
	if gc.CredType != "githubApp" {
		return g.staticCredential(ctx, gc)
	}
	u, err := url.Parse(repoURL)
	if err != nil || u.Scheme != "https" {
		return gittemplate.Credential{}, fmt.Errorf("GitCredential %q (githubApp) needs an https repoURL", gc.Name)
	}
	owner, repo, ok := ownerRepoFromPath(u.Path)
	if !ok {
		return gittemplate.Credential{}, fmt.Errorf("GitCredential %q (githubApp): repoURL path must be /<owner>/<repo>", gc.Name)
	}
	return g.controllerToken(ctx, gc, owner, repo)
}

// ForUses returns the credential for fetching a uses: template for the run of
// req.JobName. A githubApp credential is used only when its jobAccess covers
// the job's folder and the repository and grants contents read; otherwise the
// fetch is anonymous and the returned Hint explains why, so a private
// template's fetch error says that the credential does not allow it.
func (g *GitCredentialResolver) ForUses(ctx context.Context, req gittemplate.CredentialRequest) (gittemplate.Credential, error) {
	if g == nil {
		return gittemplate.Credential{}, nil
	}
	gc, err := g.st.GetGitCredentialByHost(ctx, req.Host)
	if err != nil {
		return gittemplate.Credential{}, fmt.Errorf("get git credential: %w", err)
	}
	if gc == nil {
		return gittemplate.Credential{}, nil // public repo
	}
	if gc.CredType != "githubApp" {
		return g.staticCredential(ctx, gc)
	}
	ja := gc.JobAccess
	if ja == nil {
		g.metrics.GitCredentialToken(gc.Name, "controller", "denied")
		return gittemplate.Credential{Hint: fmt.Sprintf("GitCredential %q has no jobAccess, so no job can read %s/%s through the GitHub App", gc.Name, req.Owner, req.Repo)}, nil
	}
	if !jobAccessCovers(ja.Folders, req.JobName) ||
		!dsl.RepositoryAllowed(ja.Repositories, req.Owner, req.Repo) ||
		permissionLevel(ja.Permissions["contents"]) < permissionLevel("read") {
		g.metrics.GitCredentialToken(gc.Name, "controller", "denied")
		return gittemplate.Credential{Hint: fmt.Sprintf("GitCredential %q does not allow job %q to read %s/%s through the GitHub App (jobAccess)", gc.Name, req.JobName, req.Owner, req.Repo)}, nil
	}
	return g.controllerToken(ctx, gc, req.Owner, req.Repo)
}

// JobToken issues the job-side token of a githubApp credential with
// jobAccess: its full repositories and permissions, reused while at least
// gittoken.JobMinRemaining is left. The caller checks jobAccessCovers and
// records metrics and audit.
func (g *GitCredentialResolver) JobToken(ctx context.Context, gc *store.GitCredential) (gittoken.Token, error) {
	if g == nil {
		return gittoken.Token{}, errors.New("git token issuing is not configured")
	}
	if gc == nil || gc.CredType != "githubApp" || gc.GitHubApp == nil {
		return gittoken.Token{}, errors.New("not a githubApp GitCredential")
	}
	if gc.JobAccess == nil {
		return gittoken.Token{}, fmt.Errorf("GitCredential %q has no jobAccess", gc.Name)
	}
	owner, names, err := dsl.ParseRepositoryPatterns(gc.JobAccess.Repositories)
	if err != nil {
		return gittoken.Token{}, fmt.Errorf("GitCredential %q jobAccess.%w", gc.Name, err)
	}
	return g.githubAppSource(gc, gittoken.JobMinRemaining).Token(ctx, gittoken.TokenRequest{
		Owner:        owner,
		Repositories: names,
		Permissions:  gc.JobAccess.Permissions,
	})
}

// githubAppSource returns the token source of a githubApp credential. The
// private key is decrypted only on a token cache miss.
func (g *GitCredentialResolver) githubAppSource(gc *store.GitCredential, minRemaining time.Duration) gittoken.TokenSource {
	app := gittoken.GitHubApp{
		Name:   gc.Name,
		APIURL: githubAppAPIURL(gc),
		KeyRef: gc.SecretRef,
		PrivateKey: func(ctx context.Context) ([]byte, error) {
			return g.decryptSecret(ctx, gc.SecretRef)
		},
	}
	if gc.GitHubApp != nil {
		app.AppID = gc.GitHubApp.AppID
		app.InstallationID = gc.GitHubApp.InstallationID
	}
	return g.issuer.Source(app, minRemaining)
}

// controllerToken issues a contents:read token for one repository.
func (g *GitCredentialResolver) controllerToken(ctx context.Context, gc *store.GitCredential, owner, repo string) (gittemplate.Credential, error) {
	if gc.GitHubApp == nil {
		g.metrics.GitCredentialToken(gc.Name, "controller", "error")
		return gittemplate.Credential{}, fmt.Errorf("GitCredential %q (githubApp) has no githubApp settings", gc.Name)
	}
	tok, err := g.githubAppSource(gc, gittoken.ControllerMinRemaining).Token(ctx, gittoken.TokenRequest{
		Owner:        owner,
		Repositories: []string{repo},
		Permissions:  controllerContentsRead(),
	})
	if err != nil {
		g.metrics.GitCredentialToken(gc.Name, "controller", "error")
		return gittemplate.Credential{}, err
	}
	g.metrics.GitCredentialToken(gc.Name, "controller", "ok")
	return gittemplate.Credential{Token: tok.Value, Username: gittemplate.GitHubAppUser, CacheScope: gc.Name}, nil
}

// staticCredential returns the decrypted material of a token or sshKey
// credential. Other types resolve to an anonymous Credential.
func (g *GitCredentialResolver) staticCredential(ctx context.Context, gc *store.GitCredential) (gittemplate.Credential, error) {
	switch gc.CredType {
	case "token", "sshKey":
	default:
		return gittemplate.Credential{}, nil
	}
	plaintext, err := g.decryptSecret(ctx, gc.SecretRef)
	if err != nil {
		return gittemplate.Credential{}, fmt.Errorf("GitCredential %q for host %q: %w", gc.Name, gc.Host, err)
	}
	if gc.CredType == "token" {
		return gittemplate.Credential{Token: string(plaintext), CacheScope: gc.Name}, nil
	}
	return gittemplate.Credential{SSHKey: string(plaintext), CacheScope: gc.Name}, nil
}

// decryptSecret returns the plaintext of the StoredSecret name.
func (g *GitCredentialResolver) decryptSecret(ctx context.Context, name string) ([]byte, error) {
	if g.km == nil {
		return nil, errors.New("no encryption key configured")
	}
	stored, err := g.st.GetSecret(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("get secret %q: %w", name, err)
	}
	plaintext, err := secrets.Decrypt(ctx, g.km, stored.EncryptedDEK, stored.Ciphertext, secrets.SecretBinding(stored.Name))
	if err != nil {
		return nil, fmt.Errorf("decrypt secret %q: %w", name, err)
	}
	return plaintext, nil
}

// jobAccessCovers reports whether a jobAccess folder list covers jobName:
// "*" covers every job; any other folder covers the jobs inside it (not a job
// of the same name). A root-level job is covered only by "*".
func jobAccessCovers(folders []string, jobName string) bool {
	for _, f := range folders {
		if f == "*" || authz.Covers(f, jobName) {
			return true
		}
	}
	return false
}

// permissionLevel orders GitHub App permission levels: read < write < admin.
// Anything else is 0.
func permissionLevel(level string) int {
	switch level {
	case "read":
		return 1
	case "write":
		return 2
	case "admin":
		return 3
	}
	return 0
}

// githubAppAPIURL returns the credential's API URL: the configured apiURL,
// else gittoken.DefaultAPIURL for github.com and https://<host>/api/v3
// (GitHub Enterprise Server) for any other host.
func githubAppAPIURL(gc *store.GitCredential) string {
	if gc.GitHubApp != nil && gc.GitHubApp.APIURL != "" {
		return gc.GitHubApp.APIURL
	}
	if strings.EqualFold(gc.Host, "github.com") {
		return gittoken.DefaultAPIURL
	}
	return "https://" + gc.Host + "/api/v3"
}

// repoURLHost returns the hostname of a repository URL (scheme://host/...)
// or scp-like address (user@host:path), or "" when none can be found.
func repoURLHost(repoURL string) string {
	if strings.Contains(repoURL, "://") {
		u, err := url.Parse(repoURL)
		if err != nil {
			return ""
		}
		return u.Hostname()
	}
	rest := repoURL
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	if colon := strings.Index(rest, ":"); colon > 0 {
		return strings.Trim(rest[:colon], "[]")
	}
	return ""
}

// ownerRepoFromPath splits a "/owner/repo[.git]" URL path.
func ownerRepoFromPath(p string) (owner, repo string, ok bool) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) != 2 {
		return "", "", false
	}
	owner, repo = parts[0], strings.TrimSuffix(parts[1], ".git")
	if owner == "" || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}
