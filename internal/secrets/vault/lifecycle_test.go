package vault

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAuth records logins and returns a scripted result.
type fakeAuth struct {
	mu     sync.Mutex
	logins int
	result authResult
	err    error
}

func (f *fakeAuth) login(context.Context) (authResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logins++
	return f.result, f.err
}

func (f *fakeAuth) loginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins
}

// testClock advances only when a test tells it to, so nothing waits on wall time.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Unix(1_700_000_000, 0)} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// recordingSleep captures requested delays and returns immediately, so the
// renewal loop runs at full speed under test.
type recordingSleep struct {
	mu     sync.Mutex
	delays []time.Duration
	gate   chan struct{}
}

func newRecordingSleep() *recordingSleep {
	return &recordingSleep{gate: make(chan struct{}, 1024)}
}

func (r *recordingSleep) sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	r.delays = append(r.delays, d)
	r.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r.gate <- struct{}{}:
		return nil
	}
}

func (r *recordingSleep) recorded() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.delays...)
}

func TestTokenManager_ReturnsTokenFromLogin(t *testing.T) {
	auth := &fakeAuth{result: authResult{Token: "s.tok", TTL: time.Hour, Renewable: true}}
	clock := newTestClock()
	m, err := newTokenManager(tokenManagerConfig{
		Auth:  auth,
		Renew: func(context.Context, string) (time.Duration, error) { return time.Hour, nil },
		Now:   clock.now,
		Sleep: newRecordingSleep().sleep,
	})
	require.NoError(t, err)
	t.Cleanup(m.stop)

	got, err := m.token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.tok", got)
	assert.Equal(t, 1, auth.loginCount())
}

// The renewal point is half the remaining lease, following Concourse.
func TestTokenManager_SleepsHalfTheLease(t *testing.T) {
	auth := &fakeAuth{result: authResult{Token: "s.tok", TTL: time.Hour, Renewable: true}}
	sleeper := newRecordingSleep()
	m, err := newTokenManager(tokenManagerConfig{
		Auth:  auth,
		Renew: func(context.Context, string) (time.Duration, error) { return time.Hour, nil },
		Now:   newTestClock().now,
		Sleep: sleeper.sleep,
	})
	require.NoError(t, err)
	t.Cleanup(m.stop)

	require.Eventually(t, func() bool { return len(sleeper.recorded()) > 0 },
		2*time.Second, 10*time.Millisecond)
	assert.Equal(t, 30*time.Minute, sleeper.recorded()[0])
}

// A batch token, or any token Vault will not renew, must never be sent to the
// renew endpoint — renewing it fails and would mask the real need to re-login.
func TestTokenManager_NeverRenewsNonRenewableToken(t *testing.T) {
	auth := &fakeAuth{result: authResult{Token: "b.tok", TTL: time.Hour, Renewable: false}}
	var renewCalls atomic.Int64
	sleeper := newRecordingSleep()
	m, err := newTokenManager(tokenManagerConfig{
		Auth: auth,
		Renew: func(context.Context, string) (time.Duration, error) {
			renewCalls.Add(1)
			return 0, nil
		},
		Now:   newTestClock().now,
		Sleep: sleeper.sleep,
	})
	require.NoError(t, err)
	t.Cleanup(m.stop)

	require.Eventually(t, func() bool { return auth.loginCount() >= 2 },
		2*time.Second, 10*time.Millisecond)
	assert.Zero(t, renewCalls.Load(), "a non-renewable token must be re-obtained by logging in again")
}

// A failed renewal falls back to a fresh login rather than giving up.
func TestTokenManager_ReLoginsWhenRenewalFails(t *testing.T) {
	auth := &fakeAuth{result: authResult{Token: "s.tok", TTL: time.Hour, Renewable: true}}
	sleeper := newRecordingSleep()
	m, err := newTokenManager(tokenManagerConfig{
		Auth:  auth,
		Renew: func(context.Context, string) (time.Duration, error) { return 0, errors.New("permission denied") },
		Now:   newTestClock().now,
		Sleep: sleeper.sleep,
	})
	require.NoError(t, err)
	t.Cleanup(m.stop)

	require.Eventually(t, func() bool { return auth.loginCount() >= 2 },
		2*time.Second, 10*time.Millisecond)
}

func TestTokenManager_LoginFailureIsReturnedFromConstructor(t *testing.T) {
	auth := &fakeAuth{err: errors.New("connection refused")}
	_, err := newTokenManager(tokenManagerConfig{
		Auth:  auth,
		Renew: func(context.Context, string) (time.Duration, error) { return 0, nil },
		Now:   newTestClock().now,
		Sleep: newRecordingSleep().sleep,
	})
	require.Error(t, err, "startup must fail fast when the first login fails")
	assert.Contains(t, err.Error(), "connection refused")
}

// stop must be safe to call twice — Resolved.Close may run from a defer and an
// explicit path.
func TestTokenManager_StopIsIdempotent(t *testing.T) {
	auth := &fakeAuth{result: authResult{Token: "s.tok", TTL: time.Hour, Renewable: true}}
	m, err := newTokenManager(tokenManagerConfig{
		Auth:  auth,
		Renew: func(context.Context, string) (time.Duration, error) { return time.Hour, nil },
		Now:   newTestClock().now,
		Sleep: newRecordingSleep().sleep,
	})
	require.NoError(t, err)
	m.stop()
	m.stop()
}
