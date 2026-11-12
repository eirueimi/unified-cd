package gittoken_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/gittoken"
	"github.com/eirueimi/unified-cd/internal/gittoken/gittokentest"
)

var t0 = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

// fakeClock is a settable clock shared by the issuer and the fake GitHub.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type env struct {
	fake    *gittokentest.FakeGitHub
	issuer  *gittoken.Issuer
	app     gittoken.GitHubApp
	clock   *fakeClock
	keyPEM  []byte
	keyLoad *int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	key, keyPEM := gittokentest.GenerateKeyPEM(t, false)
	fake := gittokentest.NewFakeGitHub(t, &key.PublicKey)
	clock := &fakeClock{now: t0}
	fake.Now = clock.Now
	loads := 0
	var mu sync.Mutex
	e := &env{fake: fake, clock: clock, keyPEM: keyPEM, keyLoad: &loads}
	e.issuer = gittoken.NewIssuer(fake.Client(), clock.Now)
	e.app = gittoken.GitHubApp{
		Name:           "gh-app",
		AppID:          123456,
		InstallationID: gittokentest.DefaultInstallationID,
		APIURL:         fake.URL,
		KeyRef:         "gh-app-key",
		PrivateKey: func(context.Context) ([]byte, error) {
			mu.Lock()
			loads++
			mu.Unlock()
			return keyPEM, nil
		},
	}
	return e
}

func (e *env) token(t *testing.T, app gittoken.GitHubApp, min time.Duration, req gittoken.TokenRequest) gittoken.Token {
	t.Helper()
	tok, err := e.issuer.Source(app, min).Token(context.Background(), req)
	require.NoError(t, err)
	return tok
}

func countPath(paths []string, path string) int {
	n := 0
	for _, p := range paths {
		if p == path {
			n++
		}
	}
	return n
}

func TestGitHubApp_JWTClaims(t *testing.T) {
	e := newEnv(t)
	key, keyPEM := gittokentest.GenerateKeyPEM(t, true)
	fake := gittokentest.NewFakeGitHub(t, &key.PublicKey)
	fake.Now = e.clock.Now
	app := e.app
	app.APIURL = fake.URL
	app.PrivateKey = func(context.Context) ([]byte, error) { return keyPEM, nil }
	issuer := gittoken.NewIssuer(fake.Client(), e.clock.Now)
	_, err := issuer.Source(app, gittoken.JobMinRemaining).Token(context.Background(),
		gittoken.TokenRequest{Owner: "acme", Repositories: []string{"app"}})
	require.NoError(t, err)

	tok, err := jwt.ParseSigned(fake.LastJWT(), []jose.SignatureAlgorithm{jose.RS256})
	require.NoError(t, err)
	require.Len(t, tok.Headers, 1)
	assert.Equal(t, "RS256", tok.Headers[0].Algorithm)
	var claims jwt.Claims
	require.NoError(t, tok.Claims(&key.PublicKey, &claims))
	assert.Equal(t, "123456", claims.Issuer)
	assert.Equal(t, time.Date(2026, 11, 12, 9, 59, 0, 0, time.UTC), claims.IssuedAt.Time().UTC())
	assert.Equal(t, time.Date(2026, 11, 12, 10, 9, 0, 0, time.UTC), claims.Expiry.Time().UTC())
}

func TestGitHubApp_IssuesNarrowedToken(t *testing.T) {
	e := newEnv(t)
	tok := e.token(t, e.app, gittoken.JobMinRemaining, gittoken.TokenRequest{
		Owner: "acme", Repositories: []string{"app"}, Permissions: map[string]string{"contents": "read"},
	})
	reqs := e.fake.AccessTokenRequests()
	require.Len(t, reqs, 1)
	assert.Equal(t, int64(7890123), reqs[0].InstallationID)
	assert.Equal(t, []string{"app"}, reqs[0].Repositories)
	assert.Equal(t, map[string]string{"contents": "read"}, reqs[0].Permissions)
	assert.Equal(t, []string{"/app/installations/7890123/access_tokens"}, e.fake.Paths())
	assert.Equal(t, "ghs_1", tok.Value)
	assert.True(t, tok.ExpiresAt.Equal(t0.Add(time.Hour)), "ExpiresAt %v", tok.ExpiresAt)
}

func TestGitHubApp_InstallationLookupByRepoCachedOneHour(t *testing.T) {
	e := newEnv(t)
	app := e.app
	app.InstallationID = 0
	e.token(t, app, gittoken.JobMinRemaining, gittoken.TokenRequest{Owner: "acme", Repositories: []string{"app"}})
	assert.Equal(t, 1, countPath(e.fake.Paths(), "/repos/acme/app/installation"))

	e.token(t, app, gittoken.JobMinRemaining, gittoken.TokenRequest{
		Owner: "ACME", Repositories: []string{"app"}, Permissions: map[string]string{"contents": "write"},
	})
	assert.Equal(t, 1, countPath(e.fake.Paths(), "/repos/acme/app/installation"))
	assert.Equal(t, 0, countPath(e.fake.Paths(), "/repos/ACME/app/installation"))
	require.Len(t, e.fake.AccessTokenRequests(), 2)

	e.clock.Add(61 * time.Minute)
	e.token(t, app, gittoken.JobMinRemaining, gittoken.TokenRequest{
		Owner: "acme", Repositories: []string{"app"}, Permissions: map[string]string{"issues": "read"},
	})
	assert.Equal(t, 2, countPath(e.fake.Paths(), "/repos/acme/app/installation"))
}

func TestGitHubApp_InstallationLookupByOwner(t *testing.T) {
	t.Run("organization", func(t *testing.T) {
		e := newEnv(t)
		app := e.app
		app.InstallationID = 0
		e.token(t, app, gittoken.JobMinRemaining, gittoken.TokenRequest{Owner: "acme", Repositories: []string{"*"}})
		assert.Equal(t, 1, countPath(e.fake.Paths(), "/orgs/acme/installation"))
		assert.Equal(t, 0, countPath(e.fake.Paths(), "/users/acme/installation"))
	})
	t.Run("user falls back after 404", func(t *testing.T) {
		e := newEnv(t)
		e.fake.UserAccount = true
		app := e.app
		app.InstallationID = 0
		tok := e.token(t, app, gittoken.JobMinRemaining, gittoken.TokenRequest{Owner: "acme", Repositories: []string{"*"}})
		assert.Equal(t, "ghs_1", tok.Value)
		assert.Equal(t, []string{"/orgs/acme/installation", "/users/acme/installation", "/app/installations/7890123/access_tokens"}, e.fake.Paths())
	})
}

func TestGitHubApp_StarSendsNoRepositories(t *testing.T) {
	e := newEnv(t)
	e.token(t, e.app, gittoken.JobMinRemaining, gittoken.TokenRequest{Owner: "acme", Repositories: []string{"*"}})
	reqs := e.fake.AccessTokenRequests()
	require.Len(t, reqs, 1)
	assert.False(t, reqs[0].HasRepositoriesKey)
	assert.Equal(t, 0, countPath(e.fake.Paths(), "/installation/repositories"))
}

func TestGitHubApp_PatternExpansion(t *testing.T) {
	e := newEnv(t)
	e.fake.Repos = []string{"infra-a", "Infra-B", "app"}
	for i := range 150 {
		e.fake.Repos = append(e.fake.Repos, fmt.Sprintf("svc-%03d", i))
	}
	tok := e.token(t, e.app, gittoken.JobMinRemaining, gittoken.TokenRequest{
		Owner: "acme", Repositories: []string{"infra-*"}, Permissions: map[string]string{"contents": "write"},
	})
	reqs := e.fake.AccessTokenRequests()
	require.Len(t, reqs, 2)
	assert.Equal(t, map[string]string{"metadata": "read"}, reqs[0].Permissions)
	assert.False(t, reqs[0].HasRepositoriesKey)
	assert.Equal(t, 2, countPath(e.fake.Paths(), "/installation/repositories"))
	assert.True(t, reqs[1].HasRepositoriesKey)
	assert.Equal(t, []string{"infra-a", "Infra-B"}, reqs[1].Repositories)
	assert.Equal(t, map[string]string{"contents": "write"}, reqs[1].Permissions)
	assert.Equal(t, "ghs_2", tok.Value, "the listing token is never handed out")
}

func TestIssuer_PatternWithNoMatchFails(t *testing.T) {
	e := newEnv(t)
	e.fake.Repos = []string{"app", "infra-a"}
	_, err := e.issuer.Source(e.app, gittoken.JobMinRemaining).Token(context.Background(),
		gittoken.TokenRequest{Owner: "acme", Repositories: []string{"nomatch-*"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no repository of the installation matches "nomatch-*"`)
	reqs := e.fake.AccessTokenRequests()
	require.Len(t, reqs, 1, "only the listing token is requested")
	assert.Equal(t, map[string]string{"metadata": "read"}, reqs[0].Permissions)
}

func TestIssuer_TooManyRepositoriesFails(t *testing.T) {
	e := newEnv(t)
	names := make([]string, 501)
	for i := range names {
		names[i] = fmt.Sprintf("repo-%d", i)
	}
	_, err := e.issuer.Source(e.app, gittoken.JobMinRemaining).Token(context.Background(),
		gittoken.TokenRequest{Owner: "acme", Repositories: names})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GitHub accepts at most 500 repositories per token")
	assert.Empty(t, e.fake.AccessTokenRequests())
}

func TestGitHubApp_ErrorsCarryStatusAndMessageOnly(t *testing.T) {
	e := newEnv(t)
	e.fake.IssueStatus = 422
	e.fake.IssueMessage = "Bad permissions"
	_, err := e.issuer.Source(e.app, gittoken.JobMinRemaining).Token(context.Background(),
		gittoken.TokenRequest{Owner: "acme", Repositories: []string{"app"}})
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, `github app "gh-app"`)
	assert.Contains(t, msg, "HTTP 422")
	assert.Contains(t, msg, "Bad permissions")
	require.NotEmpty(t, e.fake.LastJWT())
	assert.NotContains(t, msg, e.fake.LastJWT())
	for _, line := range strings.Split(string(e.keyPEM), "\n") {
		if line != "" {
			assert.NotContains(t, msg, line)
		}
	}
}

func TestGitHubApp_ErrorMessageTruncated(t *testing.T) {
	e := newEnv(t)
	e.fake.IssueStatus = 403
	e.fake.IssueMessage = strings.Repeat("x", 500)
	_, err := e.issuer.Source(e.app, gittoken.JobMinRemaining).Token(context.Background(),
		gittoken.TokenRequest{Owner: "acme", Repositories: []string{"app"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), strings.Repeat("x", 200))
	assert.NotContains(t, err.Error(), strings.Repeat("x", 201))
}

func TestGitHubApp_AcceptsPKCS1AndPKCS8(t *testing.T) {
	for _, pkcs8 := range []bool{false, true} {
		t.Run(fmt.Sprintf("pkcs8=%v", pkcs8), func(t *testing.T) {
			key, keyPEM := gittokentest.GenerateKeyPEM(t, pkcs8)
			fake := gittokentest.NewFakeGitHub(t, &key.PublicKey)
			app := gittoken.GitHubApp{
				Name: "gh-app", AppID: 1, InstallationID: gittokentest.DefaultInstallationID, APIURL: fake.URL, KeyRef: "k",
				PrivateKey: func(context.Context) ([]byte, error) { return keyPEM, nil },
			}
			tok, err := gittoken.NewIssuer(fake.Client(), nil).Source(app, gittoken.JobMinRemaining).Token(context.Background(),
				gittoken.TokenRequest{Owner: "acme"})
			require.NoError(t, err)
			assert.Equal(t, "ghs_1", tok.Value)
		})
	}
	t.Run("garbage", func(t *testing.T) {
		e := newEnv(t)
		garbage := "-----BEGIN RSA PRIVATE KEY-----\nTm90QUtleUJ1dFNlY3JldEJ5dGVzMDEyMzQ1Njc4OQ==\n-----END RSA PRIVATE KEY-----\n"
		app := e.app
		app.PrivateKey = func(context.Context) ([]byte, error) { return []byte(garbage), nil }
		_, err := e.issuer.Source(app, gittoken.JobMinRemaining).Token(context.Background(), gittoken.TokenRequest{Owner: "acme"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse private key")
		assert.NotContains(t, err.Error(), "Tm90QUtleUJ1dFNlY3JldEJ5dGVzMDEyMzQ1Njc4OQ")
		assert.NotContains(t, err.Error(), "NotAKeyButSecretBytes")
		assert.Empty(t, e.fake.Paths())
	})
	t.Run("not PEM", func(t *testing.T) {
		e := newEnv(t)
		app := e.app
		app.PrivateKey = func(context.Context) ([]byte, error) { return []byte("hunter2-secret"), nil }
		_, err := e.issuer.Source(app, gittoken.JobMinRemaining).Token(context.Background(), gittoken.TokenRequest{Owner: "acme"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse private key")
		assert.NotContains(t, err.Error(), "hunter2")
	})
}
