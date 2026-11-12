package gittoken_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/gittoken"
)

func TestIssuer_PrivateKeyLoadedOnlyOnMiss(t *testing.T) {
	e := newEnv(t)
	req := gittoken.TokenRequest{Owner: "acme", Repositories: []string{"app"}}
	a := e.token(t, e.app, gittoken.JobMinRemaining, req)
	b := e.token(t, e.app, gittoken.JobMinRemaining, req)
	assert.Equal(t, a, b)
	assert.Equal(t, 1, *e.keyLoad)
	assert.Len(t, e.fake.AccessTokenRequests(), 1)
}

func TestIssuer_JobTokenReissuedUnderFiftyMinutes(t *testing.T) {
	e := newEnv(t)
	req := gittoken.TokenRequest{Owner: "acme", Repositories: []string{"app"}}
	assert.Equal(t, "ghs_1", e.token(t, e.app, gittoken.JobMinRemaining, req).Value)
	e.clock.Add(9 * time.Minute)
	assert.Equal(t, "ghs_1", e.token(t, e.app, gittoken.JobMinRemaining, req).Value)
	e.clock.Add(2 * time.Minute)
	assert.Equal(t, "ghs_2", e.token(t, e.app, gittoken.JobMinRemaining, req).Value)
}

func TestIssuer_ControllerTokenReusedUntilFiveMinutes(t *testing.T) {
	e := newEnv(t)
	req := gittoken.TokenRequest{Owner: "acme", Repositories: []string{"app"}, Permissions: map[string]string{"contents": "read"}}
	assert.Equal(t, "ghs_1", e.token(t, e.app, gittoken.ControllerMinRemaining, req).Value)
	e.clock.Add(54 * time.Minute)
	assert.Equal(t, "ghs_1", e.token(t, e.app, gittoken.ControllerMinRemaining, req).Value)
	e.clock.Add(2 * time.Minute)
	assert.Equal(t, "ghs_2", e.token(t, e.app, gittoken.ControllerMinRemaining, req).Value)
}

func TestIssuer_CacheKeyIncludesConfigAndRequest(t *testing.T) {
	e := newEnv(t)
	base := gittoken.TokenRequest{Owner: "acme", Repositories: []string{"app", "lib"}, Permissions: map[string]string{"contents": "read", "issues": "write"}}
	get := func(app gittoken.GitHubApp, req gittoken.TokenRequest) string {
		return e.token(t, app, gittoken.JobMinRemaining, req).Value
	}
	assert.Equal(t, "ghs_1", get(e.app, base))

	perms := base
	perms.Permissions = map[string]string{"contents": "write", "issues": "write"}
	assert.Equal(t, "ghs_2", get(e.app, perms))

	repos := base
	repos.Repositories = []string{"app"}
	assert.Equal(t, "ghs_3", get(e.app, repos))

	appID := e.app
	appID.AppID = 999
	assert.Equal(t, "ghs_4", get(appID, base))

	keyRef := e.app
	keyRef.KeyRef = "rotated-key"
	assert.Equal(t, "ghs_5", get(keyRef, base))

	same := gittoken.TokenRequest{Owner: "ACME", Repositories: []string{"lib", "app"}, Permissions: map[string]string{}}
	same.Permissions["issues"] = "write"
	same.Permissions["contents"] = "read"
	assert.Equal(t, "ghs_1", get(e.app, same))

	star := gittoken.TokenRequest{Owner: "acme", Repositories: []string{"*"}}
	assert.Equal(t, "ghs_6", get(e.app, star))
	assert.Equal(t, "ghs_6", get(e.app, gittoken.TokenRequest{Owner: "acme"}), "nil and [\"*\"] are the same request")
}

func TestIssuer_ConcurrentMissIssuesOnce(t *testing.T) {
	e := newEnv(t)
	req := gittoken.TokenRequest{Owner: "acme", Repositories: []string{"app"}}
	src := e.issuer.Source(e.app, gittoken.JobMinRemaining)
	var wg sync.WaitGroup
	values := make([]string, 10)
	errs := make([]error, 10)
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := src.Token(context.Background(), req)
			values[i], errs[i] = tok.Value, err
		}()
	}
	wg.Wait()
	for i := range 10 {
		require.NoError(t, errs[i])
		assert.Equal(t, "ghs_1", values[i])
	}
	assert.Len(t, e.fake.AccessTokenRequests(), 1)
}

func TestIssuer_WaitingCallerHonorsContext(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	app := e.app
	app.PrivateKey = func(context.Context) ([]byte, error) {
		close(entered)
		<-release
		return e.keyPEM, nil
	}
	req := gittoken.TokenRequest{Owner: "acme"}
	src := e.issuer.Source(app, gittoken.JobMinRemaining)
	done := make(chan error, 1)
	go func() {
		_, err := src.Token(context.Background(), req)
		done <- err
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := src.Token(ctx, req)
	require.ErrorIs(t, err, context.Canceled)
	close(release)
	require.NoError(t, <-done)
}

func TestIssuer_OnRequestHook(t *testing.T) {
	e := newEnv(t)
	type call struct {
		credential string
		err        error
	}
	var mu sync.Mutex
	var calls []call
	e.issuer.OnRequest = func(credential string, err error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, call{credential, err})
	}
	e.token(t, e.app, gittoken.JobMinRemaining, gittoken.TokenRequest{Owner: "acme", Repositories: []string{"app"}})
	e.token(t, e.app, gittoken.JobMinRemaining, gittoken.TokenRequest{Owner: "acme", Repositories: []string{"app"}})
	mu.Lock()
	require.Len(t, calls, 1)
	assert.Equal(t, "gh-app", calls[0].credential)
	assert.NoError(t, calls[0].err)
	mu.Unlock()

	e.fake.IssueStatus = 500
	e.fake.IssueMessage = "boom"
	_, err := e.issuer.Source(e.app, gittoken.JobMinRemaining).Token(context.Background(),
		gittoken.TokenRequest{Owner: "acme", Repositories: []string{"lib"}})
	require.Error(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, calls, 2)
	assert.Equal(t, "gh-app", calls[1].credential)
	assert.Error(t, calls[1].err)
}

func TestIssuer_PatternWithNoMatchBesideConcreteNameFails(t *testing.T) {
	e := newEnv(t)
	e.fake.Repos = []string{"app", "infra-a"}
	_, err := e.issuer.Source(e.app, gittoken.JobMinRemaining).Token(context.Background(),
		gittoken.TokenRequest{Owner: "acme", Repositories: []string{"app", "infra-*", "nomatch-*", "other-*"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no repository of the installation matches "nomatch-*", "other-*"`)
	assert.NotContains(t, err.Error(), `"infra-*"`)
	reqs := e.fake.AccessTokenRequests()
	require.Len(t, reqs, 1, "only the listing token is requested")
	assert.Equal(t, map[string]string{"metadata": "read"}, reqs[0].Permissions)
	assert.False(t, reqs[0].HasRepositoriesKey)
}

func TestIssuer_StarMixedWithNamesRejected(t *testing.T) {
	e := newEnv(t)
	for _, repos := range [][]string{{"*", "app"}, {"app", "*"}, {"*", "infra-*"}} {
		_, err := e.issuer.Source(e.app, gittoken.JobMinRemaining).Token(context.Background(),
			gittoken.TokenRequest{Owner: "acme", Repositories: repos})
		require.Error(t, err, "%q", repos)
		assert.Contains(t, err.Error(), `mixes "*" with other names`)
	}
	assert.Empty(t, e.fake.Paths(), "nothing is sent to GitHub")
	assert.Equal(t, "ghs_1", e.token(t, e.app, gittoken.JobMinRemaining, gittoken.TokenRequest{Owner: "acme", Repositories: []string{"*", "*"}}).Value)
}

func TestIssuer_ConcreteNamesDedupedCaseInsensitively(t *testing.T) {
	e := newEnv(t)
	tok := e.token(t, e.app, gittoken.JobMinRemaining, gittoken.TokenRequest{Owner: "acme", Repositories: []string{"App", "app", "lib"}})
	reqs := e.fake.AccessTokenRequests()
	require.Len(t, reqs, 1)
	assert.Equal(t, []string{"App", "lib"}, reqs[0].Repositories)
	for _, repos := range [][]string{{"app", "App", "lib"}, {"lib", "APP", "App", "app"}, {"App", "lib"}} {
		assert.Equal(t, tok.Value, e.token(t, e.app, gittoken.JobMinRemaining, gittoken.TokenRequest{Owner: "acme", Repositories: repos}).Value, "%q", repos)
	}
	assert.Len(t, e.fake.AccessTokenRequests(), 1, "one cache key")
}
