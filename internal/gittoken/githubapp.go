package gittoken

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

const (
	maxMessageBytes  = 200
	maxBodyBytes     = 1 << 20
	maxListBodyBytes = 16 << 20
	maxListPages     = 1000
)

// parsePrivateKey parses a PEM RSA private key in PKCS#1 or PKCS#8 form. Its
// errors never include key material.
func parsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("parse private key: no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if key, ok := parsed.(*rsa.PrivateKey); ok {
			return key, nil
		}
		return nil, errors.New("parse private key: PKCS#8 key is not an RSA key")
	}
	return nil, errors.New("parse private key: not a PKCS#1 or PKCS#8 RSA private key")
}

// signAppJWT returns the RS256 App JWT GitHub expects: iss = App ID,
// iat = now - 60 s (clock drift), exp = now + 9 min (GitHub's limit is 10).
func signAppJWT(key *rsa.PrivateKey, appID int64, now time.Time) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		return "", errors.New("create JWT signer")
	}
	s, err := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer:   strconv.FormatInt(appID, 10),
		IssuedAt: jwt.NewNumericDate(now.Add(-60 * time.Second)),
		Expiry:   jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).Serialize()
	if err != nil {
		return "", errors.New("sign JWT")
	}
	return s, nil
}

// apiError is a non-2xx GitHub response. It carries only the status and
// GitHub's message.
type apiError struct {
	credential string
	op         string
	status     int
	message    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("github app %q: %s: HTTP %d: %s", e.credential, e.op, e.status, e.message)
}

// githubAPI makes the GitHub REST calls for one credential.
type githubAPI struct {
	client     *http.Client
	base       *url.URL // validated https API URL without a trailing slash
	credential string
}

func newGitHubAPI(client *http.Client, app GitHubApp) (*githubAPI, error) {
	raw := strings.TrimRight(app.APIURL, "/")
	if raw == "" {
		raw = DefaultAPIURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("github app %q: API URL must be an https URL", app.Name)
	}
	return &githubAPI{client: client, base: u, credential: app.Name}, nil
}

func (a *githubAPI) url(path string) string { return a.base.String() + path }

func (a *githubAPI) errorf(op string, format string, args ...any) error {
	return fmt.Errorf("github app %q: %s: %s", a.credential, op, fmt.Sprintf(format, args...))
}

// do sends one request. auth is the full Authorization header value. On a
// 2xx response the body is decoded into out (when non-nil) and the response
// headers are returned.
func (a *githubAPI) do(ctx context.Context, op, method, target, auth string, body any, out any, limit int64) (http.Header, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, a.errorf(op, "encode request")
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, a.errorf(op, "build request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", auth)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		// *url.Error names the method and URL only; neither carries a secret.
		return nil, fmt.Errorf("github app %q: %s: %w", a.credential, op, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("github app %q: %s: read response: %w", a.credential, op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &apiError{credential: a.credential, op: op, status: resp.StatusCode, message: githubMessage(data, resp.StatusCode)}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, a.errorf(op, "decode response")
		}
	}
	return resp.Header, nil
}

// githubMessage extracts GitHub's "message", truncated to maxMessageBytes.
func githubMessage(body []byte, status int) string {
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &e)
	msg := strings.Join(strings.Fields(e.Message), " ")
	if msg == "" {
		msg = http.StatusText(status)
	}
	if len(msg) > maxMessageBytes {
		msg = strings.ToValidUTF8(msg[:maxMessageBytes], "")
	}
	return msg
}

// lookupInstallation finds the App's installation for owner: from the first
// concrete repository name when there is one, otherwise from the
// organization, falling back to the user account on 404.
func (a *githubAPI) lookupInstallation(ctx context.Context, appJWT, owner string, concrete []string) (int64, error) {
	op := fmt.Sprintf("look up installation for %q", owner)
	var out struct {
		ID int64 `json:"id"`
	}
	auth := "Bearer " + appJWT
	if len(concrete) > 0 {
		path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(concrete[0]) + "/installation"
		if _, err := a.do(ctx, op, http.MethodGet, a.url(path), auth, nil, &out, maxBodyBytes); err != nil {
			return 0, err
		}
	} else {
		_, err := a.do(ctx, op, http.MethodGet, a.url("/orgs/"+url.PathEscape(owner)+"/installation"), auth, nil, &out, maxBodyBytes)
		var ae *apiError
		if errors.As(err, &ae) && ae.status == http.StatusNotFound {
			_, err = a.do(ctx, op, http.MethodGet, a.url("/users/"+url.PathEscape(owner)+"/installation"), auth, nil, &out, maxBodyBytes)
		}
		if err != nil {
			return 0, err
		}
	}
	if out.ID <= 0 {
		return 0, a.errorf(op, "response has no installation id")
	}
	return out.ID, nil
}

// createToken requests an installation access token. A nil repositories
// slice omits the field (the installation's repositories); empty
// permissions are omitted (the installation's permissions).
func (a *githubAPI) createToken(ctx context.Context, appJWT string, installationID int64, repositories []string, permissions map[string]string) (Token, error) {
	const op = "create installation token"
	body := map[string]any{}
	if repositories != nil {
		body["repositories"] = repositories
	}
	if len(permissions) > 0 {
		body["permissions"] = permissions
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := "/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	if _, err := a.do(ctx, op, http.MethodPost, a.url(path), "Bearer "+appJWT, body, &out, maxBodyBytes); err != nil {
		return Token{}, err
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return Token{}, a.errorf(op, "response has no token or expiry")
	}
	return Token{Value: out.Token, ExpiresAt: out.ExpiresAt}, nil
}

// listRepositories returns the names of every repository the installation
// token can access, following pagination to the end.
func (a *githubAPI) listRepositories(ctx context.Context, token string) ([]string, error) {
	const op = "list installation repositories"
	next := a.url("/installation/repositories?per_page=100")
	var names []string
	for page := 0; next != ""; page++ {
		if page == maxListPages {
			return nil, a.errorf(op, "more than %d pages", maxListPages)
		}
		var out struct {
			Repositories []struct {
				Name string `json:"name"`
			} `json:"repositories"`
		}
		header, err := a.do(ctx, op, http.MethodGet, next, "token "+token, nil, &out, maxListBodyBytes)
		if err != nil {
			return nil, err
		}
		for _, r := range out.Repositories {
			names = append(names, r.Name)
		}
		next, err = a.nextLink(header.Get("Link"))
		if err != nil {
			return nil, a.errorf(op, "%s", err.Error())
		}
	}
	return names, nil
}

// nextLink returns the rel="next" URL of a Link header, or "" when there is
// none. The URL must stay on the API URL's scheme and host so the token is
// never sent elsewhere.
func (a *githubAPI) nextLink(header string) (string, error) {
	for _, part := range strings.Split(header, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok {
			continue
		}
		isNext := false
		for _, p := range strings.Split(params, ";") {
			if strings.TrimSpace(p) == `rel="next"` {
				isNext = true
			}
		}
		if !isNext {
			continue
		}
		target = strings.TrimSpace(target)
		if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
			return "", errors.New("malformed pagination link")
		}
		u, err := url.Parse(target[1 : len(target)-1])
		if err != nil {
			return "", errors.New("malformed pagination link")
		}
		u = a.base.ResolveReference(u)
		if u.Scheme != a.base.Scheme || !strings.EqualFold(u.Host, a.base.Host) {
			return "", errors.New("pagination link points outside the API URL")
		}
		return u.String(), nil
	}
	return "", nil
}

// isPrefixPattern reports whether a repository entry is a trailing-"*"
// prefix pattern (as opposed to "*" or a concrete name).
func isPrefixPattern(name string) bool {
	return name != "*" && strings.HasSuffix(name, "*")
}

// expandRepositories resolves the requested entries against the
// installation's repositories: concrete names are kept as given, prefix
// patterns are replaced by the matching names. Every prefix pattern must
// match at least one repository, and the result is never empty, so a token
// is never requested for less (or more) than was asked for.
func expandRepositories(credential string, requested, installation []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if k := strings.ToLower(name); !seen[k] {
			seen[k] = true
			out = append(out, name)
		}
	}
	var patterns []string
	for _, r := range requested {
		if isPrefixPattern(r) {
			patterns = append(patterns, r)
		} else {
			add(r)
		}
	}
	matched := make([]bool, len(patterns))
	for _, name := range installation {
		hit := false
		for i, p := range patterns {
			if dsl.MatchRepositoryName(p, name) {
				matched[i], hit = true, true
			}
		}
		if hit {
			add(name)
		}
	}
	var unmatched []string
	for i, p := range patterns {
		if !matched[i] {
			unmatched = append(unmatched, strconv.Quote(p))
		}
	}
	if len(unmatched) > 0 {
		return nil, fmt.Errorf("github app %q: no repository of the installation matches %s", credential, strings.Join(unmatched, ", "))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("github app %q: no repository of the installation matches the request", credential)
	}
	return out, nil
}
