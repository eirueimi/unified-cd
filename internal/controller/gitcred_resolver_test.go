package controller

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
	"github.com/eirueimi/unified-cd/internal/gittoken"
	"github.com/eirueimi/unified-cd/internal/gittoken/gittokentest"
	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/eirueimi/unified-cd/internal/store/storetest"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var gitCredTestNow = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

// gitCredEnv is a store, key manager, fake GitHub and resolver wired together.
type gitCredEnv struct {
	st      store.Store
	km      secrets.KeyManager
	fake    *gittokentest.FakeGitHub
	metrics *metrics.Metrics
	creds   *GitCredentialResolver
	keyPEM  []byte
}

func newGitCredEnv(t *testing.T) *gitCredEnv {
	t.Helper()
	pg := storetest.NewTestPostgres(t)
	km := testKeyManager(t)
	key, keyPEM := gittokentest.GenerateKeyPEM(t, false)
	fake := gittokentest.NewFakeGitHub(t, &key.PublicKey)
	fake.Now = func() time.Time { return gitCredTestNow }
	issuer := gittoken.NewIssuer(fake.Client(), func() time.Time { return gitCredTestNow })
	m := metrics.New()
	return &gitCredEnv{
		st: pg, km: km, fake: fake, metrics: m, keyPEM: keyPEM,
		creds: NewGitCredentialResolver(pg, km, issuer, m),
	}
}

func (e *gitCredEnv) putSecret(t *testing.T, name string, value []byte) {
	t.Helper()
	dek, ct, err := secrets.Encrypt(context.Background(), e.km, value, secrets.SecretBinding(name))
	require.NoError(t, err)
	_, err = e.st.UpsertSecret(context.Background(), name, dek, ct)
	require.NoError(t, err)
}

// putGitHubApp stores a githubApp credential named gh-app for github.com
// pointing at the fake GitHub, with the given jobAccess.
func (e *gitCredEnv) putGitHubApp(t *testing.T, jobAccess *api.GitJobAccess) {
	t.Helper()
	e.putSecret(t, "gh-app-key", e.keyPEM)
	require.NoError(t, e.st.UpsertGitCredential(context.Background(), store.GitCredential{
		Name: "gh-app", Host: "github.com", CredType: "githubApp", SecretRef: "gh-app-key",
		ControllerUse: true,
		GitHubApp: &api.GitHubAppConfig{
			AppID: 12345, InstallationID: gittokentest.DefaultInstallationID, APIURL: e.fake.URL,
		},
		JobAccess: jobAccess,
	}))
}

func TestJobAccessCovers(t *testing.T) {
	assert.True(t, jobAccessCovers([]string{"team-a"}, "team-a/build"))
	assert.True(t, jobAccessCovers([]string{"platform/tools"}, "platform/tools/x/y"))
	assert.False(t, jobAccessCovers([]string{"team-a"}, "team-ab/x"))
	assert.False(t, jobAccessCovers([]string{"team-a"}, "team-a")) // a folder covers jobs inside it, not a job of that name
	assert.False(t, jobAccessCovers([]string{"team-a"}, "build"))  // root-level
	assert.True(t, jobAccessCovers([]string{"*"}, "build"))
	assert.False(t, jobAccessCovers(nil, "team-a/build"))
	assert.True(t, jobAccessCovers([]string{"team-b", "team-a"}, "team-a/build"))
}

func TestGitCredResolver_NilResolverIsAnonymous(t *testing.T) {
	var g *GitCredentialResolver
	cred, err := g.ForAppSource(context.Background(), "https://github.com/acme/app.git")
	require.NoError(t, err)
	assert.Equal(t, gittemplate.Credential{}, cred)
	cred, err = g.ForUses(context.Background(), gittemplate.CredentialRequest{Host: "github.com", Owner: "acme", Repo: "ci", JobName: "a/b"})
	require.NoError(t, err)
	assert.Equal(t, gittemplate.Credential{}, cred)
}

func TestGitCredResolver_TokenAndSSHUnchanged(t *testing.T) {
	e := newGitCredEnv(t)
	ctx := context.Background()
	e.putSecret(t, "pat", []byte("glpat-secret"))
	require.NoError(t, e.st.UpsertGitCredential(ctx, store.GitCredential{
		Name: "gitlab-pat", Host: "gitlab.example.com", CredType: "token", SecretRef: "pat", ControllerUse: true,
	}))
	e.putSecret(t, "deploy-key", []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nx\n"))
	require.NoError(t, e.st.UpsertGitCredential(ctx, store.GitCredential{
		Name: "ssh-cred", Host: "git.example.com", CredType: "sshKey", SecretRef: "deploy-key", ControllerUse: true,
	}))

	cred, err := e.creds.ForAppSource(ctx, "https://gitlab.example.com/acme/app.git")
	require.NoError(t, err)
	assert.Equal(t, gittemplate.Credential{Token: "glpat-secret", CacheScope: "gitlab-pat"}, cred)

	cred, err = e.creds.ForUses(ctx, gittemplate.CredentialRequest{Host: "gitlab.example.com", Owner: "acme", Repo: "ci", JobName: "build"})
	require.NoError(t, err)
	assert.Equal(t, gittemplate.Credential{Token: "glpat-secret", CacheScope: "gitlab-pat"}, cred)

	cred, err = e.creds.ForAppSource(ctx, "git@git.example.com:acme/app.git")
	require.NoError(t, err)
	assert.Equal(t, "-----BEGIN OPENSSH PRIVATE KEY-----\nx\n", cred.SSHKey)
	assert.Equal(t, "ssh-cred", cred.CacheScope)
	assert.Empty(t, cred.Token)

	cred, err = e.creds.ForUses(ctx, gittemplate.CredentialRequest{Host: "git.example.com", Owner: "acme", Repo: "ci", JobName: "build"})
	require.NoError(t, err)
	assert.NotEmpty(t, cred.SSHKey)
	assert.Equal(t, "ssh-cred", cred.CacheScope)

	// No credential for the host: anonymous.
	cred, err = e.creds.ForAppSource(ctx, "https://public.example.com/acme/app.git")
	require.NoError(t, err)
	assert.Equal(t, gittemplate.Credential{}, cred)
	assert.Empty(t, e.fake.Paths(), "token and sshKey credentials never call GitHub")
}

func TestGitCredResolver_AppSourceGitHubApp(t *testing.T) {
	e := newGitCredEnv(t)
	// jobAccess does not cover anything the controller fetches; it must not be consulted.
	e.putGitHubApp(t, &api.GitJobAccess{Folders: []string{"team-z"}, Repositories: []string{"other/x"}, Permissions: map[string]string{"statuses": "write"}})

	cred, err := e.creds.ForAppSource(context.Background(), "https://github.com/acme/app.git")
	require.NoError(t, err)
	assert.Equal(t, gittemplate.Credential{Token: "ghs_1", Username: gittemplate.GitHubAppUser, CacheScope: "gh-app"}, cred)

	reqs := e.fake.AccessTokenRequests()
	require.Len(t, reqs, 1)
	assert.Equal(t, []string{"app"}, reqs[0].Repositories)
	assert.Equal(t, map[string]string{"contents": "read"}, reqs[0].Permissions)

	// Cached: a second sync reuses the token.
	cred, err = e.creds.ForAppSource(context.Background(), "https://github.com/acme/app")
	require.NoError(t, err)
	assert.Equal(t, "ghs_1", cred.Token)
	assert.Len(t, e.fake.AccessTokenRequests(), 1)
}

func TestGitCredResolver_AppSourceGitHubAppRequiresHTTPS(t *testing.T) {
	e := newGitCredEnv(t)
	e.putGitHubApp(t, nil)
	for _, repoURL := range []string{"git@github.com:acme/app.git", "ssh://git@github.com/acme/app.git"} {
		_, err := e.creds.ForAppSource(context.Background(), repoURL)
		require.Error(t, err, repoURL)
		assert.Contains(t, err.Error(), `GitCredential "gh-app" (githubApp) needs an https repoURL`)
	}
	assert.Empty(t, e.fake.Paths())
}

func TestGitCredResolver_AppSourceGitHubAppNeedsOwnerRepo(t *testing.T) {
	e := newGitCredEnv(t)
	e.putGitHubApp(t, nil)
	_, err := e.creds.ForAppSource(context.Background(), "https://github.com/acme")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `GitCredential "gh-app"`)
	assert.Empty(t, e.fake.Paths())
}

func TestGitCredResolver_UsesJobAccess(t *testing.T) {
	base := &api.GitJobAccess{Folders: []string{"team-a"}, Repositories: []string{"acme/*"}, Permissions: map[string]string{"contents": "read"}}
	all := &api.GitJobAccess{Folders: []string{"*"}, Repositories: []string{"acme/*"}, Permissions: map[string]string{"contents": "read"}}
	statusesOnly := &api.GitJobAccess{Folders: []string{"team-a"}, Repositories: []string{"acme/*"}, Permissions: map[string]string{"statuses": "write"}}
	writer := &api.GitJobAccess{Folders: []string{"team-a"}, Repositories: []string{"acme/ci"}, Permissions: map[string]string{"contents": "write", "statuses": "write"}}
	cases := []struct {
		name      string
		access    *api.GitJobAccess
		job       string
		owner     string
		repo      string
		wantToken bool
		wantHint  string
	}{
		{name: "allowed", access: base, job: "team-a/build", owner: "acme", repo: "ci", wantToken: true},
		{name: "owner case-insensitive", access: base, job: "team-a/build", owner: "Acme", repo: "ci", wantToken: true},
		{name: "contents write covers read", access: writer, job: "team-a/build", owner: "acme", repo: "ci", wantToken: true},
		{name: "other folder", access: base, job: "team-b/x", owner: "acme", repo: "ci",
			wantHint: `GitCredential "gh-app" does not allow job "team-b/x" to read acme/ci through the GitHub App (jobAccess)`},
		{name: "root-level job", access: base, job: "build", owner: "acme", repo: "ci",
			wantHint: `GitCredential "gh-app" does not allow job "build" to read acme/ci through the GitHub App (jobAccess)`},
		{name: "star folder covers root", access: all, job: "build", owner: "acme", repo: "ci", wantToken: true},
		{name: "other owner", access: base, job: "team-a/build", owner: "widget", repo: "ci",
			wantHint: `GitCredential "gh-app" does not allow job "team-a/build" to read widget/ci through the GitHub App (jobAccess)`},
		{name: "repo not listed", access: writer, job: "team-a/build", owner: "acme", repo: "app",
			wantHint: `GitCredential "gh-app" does not allow job "team-a/build" to read acme/app through the GitHub App (jobAccess)`},
		{name: "no contents permission", access: statusesOnly, job: "team-a/build", owner: "acme", repo: "ci",
			wantHint: `GitCredential "gh-app" does not allow job "team-a/build" to read acme/ci through the GitHub App (jobAccess)`},
		{name: "no jobAccess", access: nil, job: "team-a/build", owner: "acme", repo: "ci",
			wantHint: `GitCredential "gh-app" has no jobAccess, so no job can read acme/ci through the GitHub App`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newGitCredEnv(t)
			e.putGitHubApp(t, tc.access)
			cred, err := e.creds.ForUses(context.Background(), gittemplate.CredentialRequest{
				Host: "github.com", Owner: tc.owner, Repo: tc.repo, JobName: tc.job,
			})
			require.NoError(t, err)
			if tc.wantToken {
				assert.Equal(t, gittemplate.Credential{Token: "ghs_1", Username: gittemplate.GitHubAppUser, CacheScope: "gh-app"}, cred)
				reqs := e.fake.AccessTokenRequests()
				require.Len(t, reqs, 1)
				assert.Equal(t, []string{tc.repo}, reqs[0].Repositories, "narrowed to the template repository")
				assert.Equal(t, map[string]string{"contents": "read"}, reqs[0].Permissions)
				return
			}
			assert.Empty(t, cred.Token)
			assert.Empty(t, cred.SSHKey)
			assert.Empty(t, cred.CacheScope, "an anonymous fetch reads the anonymous cache scope only")
			assert.Equal(t, tc.wantHint, cred.Hint)
			assert.Empty(t, e.fake.Paths(), "a denied job never calls GitHub")
			assert.Equal(t, 1.0, testutil.ToFloat64(e.metrics.GitCredentialTokenCounterForTest("gh-app", "controller", "denied")))
		})
	}
}

func TestGitCredResolver_ControllerUseFalseIgnored(t *testing.T) {
	e := newGitCredEnv(t)
	e.putSecret(t, "pat", []byte("ghp_secret"))
	require.NoError(t, e.st.UpsertGitCredential(context.Background(), store.GitCredential{
		Name: "job-only", Host: "github.com", CredType: "token", SecretRef: "pat", ControllerUse: false,
	}))
	cred, err := e.creds.ForAppSource(context.Background(), "https://github.com/acme/app.git")
	require.NoError(t, err)
	assert.Equal(t, gittemplate.Credential{}, cred)
	cred, err = e.creds.ForUses(context.Background(), gittemplate.CredentialRequest{Host: "github.com", Owner: "acme", Repo: "ci", JobName: "a/b"})
	require.NoError(t, err)
	assert.Equal(t, gittemplate.Credential{}, cred)
}

func TestGitCredResolver_Metrics(t *testing.T) {
	e := newGitCredEnv(t)
	e.putGitHubApp(t, nil)
	_, err := e.creds.ForAppSource(context.Background(), "https://github.com/acme/app.git")
	require.NoError(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(e.metrics.GitCredentialTokenCounterForTest("gh-app", "controller", "ok")))

	e.fake.IssueStatus = http.StatusInternalServerError
	e.fake.IssueMessage = "boom"
	_, err = e.creds.ForAppSource(context.Background(), "https://github.com/acme/other.git")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	assert.NotContains(t, err.Error(), string(e.keyPEM))
	assert.Equal(t, 1.0, testutil.ToFloat64(e.metrics.GitCredentialTokenCounterForTest("gh-app", "controller", "error")))
}

func TestGitCredResolver_JobToken(t *testing.T) {
	e := newGitCredEnv(t)
	e.putGitHubApp(t, &api.GitJobAccess{
		Folders: []string{"team-a"}, Repositories: []string{"acme/app", "acme/infra"},
		Permissions: map[string]string{"contents": "write", "statuses": "write"},
	})
	gc, err := e.st.GetGitCredential(context.Background(), "gh-app")
	require.NoError(t, err)
	tok, err := e.creds.JobToken(context.Background(), gc)
	require.NoError(t, err)
	assert.Equal(t, "ghs_1", tok.Value)
	assert.Equal(t, gitCredTestNow.Add(time.Hour), tok.ExpiresAt)
	reqs := e.fake.AccessTokenRequests()
	require.Len(t, reqs, 1)
	assert.Equal(t, []string{"app", "infra"}, reqs[0].Repositories)
	assert.Equal(t, map[string]string{"contents": "write", "statuses": "write"}, reqs[0].Permissions)

	// A token credential has no job token.
	_, err = e.creds.JobToken(context.Background(), &store.GitCredential{Name: "pat", CredType: "token"})
	require.Error(t, err)
	// Nor does a githubApp credential without jobAccess.
	_, err = e.creds.JobToken(context.Background(), &store.GitCredential{Name: "x", CredType: "githubApp", GitHubApp: &api.GitHubAppConfig{AppID: 1}})
	require.Error(t, err)
}

func TestGitHubAppAPIURL(t *testing.T) {
	assert.Equal(t, gittoken.DefaultAPIURL, githubAppAPIURL(&store.GitCredential{Host: "github.com", GitHubApp: &api.GitHubAppConfig{}}))
	assert.Equal(t, "https://ghe.example.com/api/v3", githubAppAPIURL(&store.GitCredential{Host: "ghe.example.com", GitHubApp: &api.GitHubAppConfig{}}))
	assert.Equal(t, "https://api.example.com", githubAppAPIURL(&store.GitCredential{Host: "ghe.example.com", GitHubApp: &api.GitHubAppConfig{APIURL: "https://api.example.com"}}))
}

func TestResolveGitPendingRuns_PassesJobName(t *testing.T) {
	e := newGitCredEnv(t)
	ctx := context.Background()
	e.putGitHubApp(t, &api.GitJobAccess{Folders: []string{"team-a"}, Repositories: []string{"acme/*"}, Permissions: map[string]string{"contents": "read"}})
	_, err := e.st.UpsertJob(ctx, "team-a/build", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := e.st.CreateRun(ctx, "team-a/build", nil, []byte(`{"steps":[{"name":"tpl","uses":{"job":"git://github.com/acme/ci/x.yaml@v1"}}]}`), nil, nil, "", "")
	require.NoError(t, err)

	fetcher := &credRecordingFetcher{yaml: []byte(simpleTemplate)}
	resolver := gittemplate.NewResolver(fetcher, nil)
	resolveGitPendingRuns(ctx, e.st, resolver, e.creds, newFailureBackoff(time.Minute, time.Hour, 10_000), time.Hour)

	require.Len(t, fetcher.creds, 1)
	assert.Equal(t, "ghs_1", fetcher.creds[0].Token, "the job's folder is allowed, so the fetch is authenticated")
	gotSpec, err := e.st.GetRunSpec(ctx, run.ID)
	require.NoError(t, err)
	assert.NotContains(t, string(gotSpec), "git://", "the uses: step was resolved")
}

func TestResolveGitPendingRuns_TemplateCacheDoesNotLeakAcrossJobAccess(t *testing.T) {
	e := newGitCredEnv(t)
	ctx := context.Background()
	e.putGitHubApp(t, &api.GitJobAccess{Folders: []string{"team-a"}, Repositories: []string{"acme/*"}, Permissions: map[string]string{"contents": "read"}})
	spec := []byte(`{"steps":[{"name":"tpl","uses":{"job":"git://github.com/acme/ci/x.yaml@v1"}}]}`)
	for _, job := range []string{"team-a/build", "team-b/x"} {
		_, err := e.st.UpsertJob(ctx, job, "unified-cd/v1", []byte(`{}`))
		require.NoError(t, err)
	}

	// A private repository: only an authenticated fetch succeeds.
	fetcher := &credRecordingFetcher{yaml: []byte(simpleTemplate), requireToken: true}
	cache := gittemplate.NewCache(objectstore.NewLocalObjectStore(t.TempDir()))
	resolver := gittemplate.NewResolver(fetcher, cache)
	bo := newFailureBackoff(time.Minute, time.Hour, 10_000)

	allowed, err := e.st.CreateRun(ctx, "team-a/build", nil, spec, nil, nil, "", "")
	require.NoError(t, err)
	resolveGitPendingRuns(ctx, e.st, resolver, e.creds, bo, time.Hour)
	gotSpec, err := e.st.GetRunSpec(ctx, allowed.ID)
	require.NoError(t, err)
	require.NotContains(t, string(gotSpec), "git://", "run 1 resolves and populates the cache")
	require.Len(t, fetcher.creds, 1)

	denied, err := e.st.CreateRun(ctx, "team-b/x", nil, spec, nil, nil, "", "")
	require.NoError(t, err)
	resolveGitPendingRuns(ctx, e.st, resolver, e.creds, bo, time.Hour)
	got, err := e.st.GetRun(ctx, denied.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunPending, got.Status)
	gotSpec, err = e.st.GetRunSpec(ctx, denied.ID)
	require.NoError(t, err)
	assert.Contains(t, string(gotSpec), "git://", "run 2 must not be resolved from run 1's cached template")
	require.Len(t, fetcher.creds, 2, "run 2's fetch is attempted, not served from the cache")
	assert.Empty(t, fetcher.creds[1].Token, "run 2 fetches anonymously")

	// The fetch error carries the jobAccess hint.
	_, err = resolver.ResolveSpec(ctx, spec, func(ctx context.Context, req gittemplate.CredentialRequest) (gittemplate.Credential, error) {
		req.JobName = "team-b/x"
		return e.creds.ForUses(ctx, req)
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `GitCredential "gh-app" does not allow job "team-b/x" to read acme/ci through the GitHub App (jobAccess)`)
}

const simpleTemplate = "apiVersion: unified-cd/v1\nkind: JobTemplate\nmetadata: {name: tmpl}\nspec:\n  steps:\n    - {name: s, run: echo hi}\n"

// credRecordingFetcher records every fetch's credential; with requireToken it
// fails anonymous fetches like git does for a private repository.
type credRecordingFetcher struct {
	yaml         []byte
	requireToken bool
	creds        []gittemplate.Credential
}

func (f *credRecordingFetcher) Fetch(_ context.Context, _ gittemplate.URI, cred gittemplate.Credential) ([]byte, error) {
	f.creds = append(f.creds, cred)
	if f.requireToken && cred.Token == "" {
		return nil, errors.New("fatal: could not read Username for 'https://github.com': terminal prompts disabled")
	}
	return f.yaml, nil
}
