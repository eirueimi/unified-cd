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
//
// If notify is set, the notifyAt'th call parks: it hands off to the test
// through notify and then blocks until ctx is done. This lets a test that
// only needs to observe the Nth login stop the manager the instant it has
// what it needs, instead of racing require.Eventually's poll interval
// against a renewal loop whose injected sleep returns immediately — a race
// the loop wins by spinning (and, on the renewal-failure path, logging)
// thousands of times before the test notices.
type fakeAuth struct {
	mu       sync.Mutex
	logins   int
	result   authResult
	err      error
	notify   chan struct{}
	notifyAt int
}

func (f *fakeAuth) login(ctx context.Context) (authResult, error) {
	f.mu.Lock()
	f.logins++
	n := f.logins
	f.mu.Unlock()
	if f.notify != nil && n == f.notifyAt {
		select {
		case f.notify <- struct{}{}:
		case <-ctx.Done():
			return authResult{}, ctx.Err()
		}
		// Stay parked until the test stops the manager, so the loop cannot
		// take another lap (and log again) while the test is busy stopping it.
		<-ctx.Done()
		return authResult{}, ctx.Err()
	}
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
	notify := make(chan struct{})
	auth := &fakeAuth{result: authResult{Token: "b.tok", TTL: time.Hour, Renewable: false}, notify: notify, notifyAt: 2}
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

	select {
	case <-notify:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the second login")
	}
	m.stop()

	assert.GreaterOrEqual(t, auth.loginCount(), 2)
	assert.Zero(t, renewCalls.Load(), "a non-renewable token must be re-obtained by logging in again")
}

// A failed renewal falls back to a fresh login rather than giving up.
//
// The renewal loop's injected sleep returns immediately (see recordingSleep),
// so nothing throttles retries. Left to require.Eventually's poll interval,
// the loop would keep relogging — and logging a slog.Warn per attempt — for
// as long as it takes the poller to notice, flooding test output. Instead,
// fakeAuth's notify hands off the instant the second login happens and the
// fake parks, so the loop cannot take another lap before the test stops it.
func TestTokenManager_ReLoginsWhenRenewalFails(t *testing.T) {
	notify := make(chan struct{})
	auth := &fakeAuth{result: authResult{Token: "s.tok", TTL: time.Hour, Renewable: true}, notify: notify, notifyAt: 2}
	sleeper := newRecordingSleep()
	m, err := newTokenManager(tokenManagerConfig{
		Auth:  auth,
		Renew: func(context.Context, string) (time.Duration, error) { return 0, errors.New("permission denied") },
		Now:   newTestClock().now,
		Sleep: sleeper.sleep,
	})
	require.NoError(t, err)
	t.Cleanup(m.stop)

	select {
	case <-notify:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the second login")
	}
	m.stop()

	assert.GreaterOrEqual(t, auth.loginCount(), 2, "a failed renewal must fall back to a fresh login")
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

// flakyAuth's login succeeds on the first call (satisfying the constructor's
// fail-fast login), fails on the next `failures` calls (simulating an
// outage), then succeeds forever after with ttl.
type flakyAuth struct {
	mu       sync.Mutex
	calls    int
	failures int
	ttl      time.Duration
	err      error
}

func (f *flakyAuth) login(context.Context) (authResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls > 1 && f.calls <= 1+f.failures {
		return authResult{}, f.err
	}
	return authResult{Token: "s.tok", TTL: f.ttl, Renewable: false}, nil
}

func (f *flakyAuth) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// scriptedResult is one login outcome in a scriptedAuth's script.
type scriptedResult struct {
	result authResult
	err    error
}

// scriptedAuth returns login outcomes from a fixed script, one per call,
// repeating the final outcome once the script is exhausted. Used to drive
// outage patterns more precisely than flakyAuth's single fail-streak, e.g.
// "fail a few times, succeed, fail again."
type scriptedAuth struct {
	mu     sync.Mutex
	calls  int
	script []scriptedResult
}

func (a *scriptedAuth) login(context.Context) (authResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	idx := a.calls
	if idx >= len(a.script) {
		idx = len(a.script) - 1
	}
	a.calls++
	r := a.script[idx]
	return r.result, r.err
}

func (a *scriptedAuth) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// Regression guard for the geometric-decay defect: on repeated refresh
// failures, the retry wait must climb (retryInitialWait, 2x, 4x, ...), never
// shrink toward zero. Against the old `current = authResult{TTL: wait}`
// behaviour this fails: the retry delays halve every time (10m, 5m, 2.5m,
// 1.25m, 0.625m), which is both decreasing and eventually below
// retryInitialWait.
func TestTokenManager_FailuresBackOffNotDown(t *testing.T) {
	auth := &flakyAuth{ttl: 20 * time.Minute, failures: 4, err: errors.New("vault unreachable")}
	sleeper := newRecordingSleep()
	m, err := newTokenManager(tokenManagerConfig{
		Auth: auth,
		Renew: func(context.Context, string) (time.Duration, error) {
			return 0, errors.New("unused: token is not renewable")
		},
		Now:   newTestClock().now,
		Sleep: sleeper.sleep,
	})
	require.NoError(t, err)
	t.Cleanup(m.stop)

	// initial login (1) + 4 scripted failures (2..5) + the recovering login (6)
	require.Eventually(t, func() bool { return auth.callCount() >= 6 },
		2*time.Second, time.Millisecond)

	delays := sleeper.recorded()
	require.GreaterOrEqual(t, len(delays), 5, "expected the lease-half wait plus 4 retry waits")

	// delays[0] is the success-cadence half-lease sleep; delays[1:5] are the
	// four retry backoff sleeps.
	retries := delays[1:5]
	for i, d := range retries {
		assert.GreaterOrEqualf(t, d, retryInitialWait, "retry %d (%s) fell below retryInitialWait", i, d)
	}
	for i := 1; i < len(retries); i++ {
		assert.GreaterOrEqualf(t, retries[i], retries[i-1],
			"retry delays must not decrease across consecutive failures: %v", retries)
	}
}

// After enough consecutive failures the backoff must stop growing and hold
// at retryMaxWait.
func TestTokenManager_BackoffCapsAtMax(t *testing.T) {
	auth := &flakyAuth{ttl: 20 * time.Minute, failures: 7, err: errors.New("vault unreachable")}
	sleeper := newRecordingSleep()
	m, err := newTokenManager(tokenManagerConfig{
		Auth: auth,
		Renew: func(context.Context, string) (time.Duration, error) {
			return 0, errors.New("unused: token is not renewable")
		},
		Now:   newTestClock().now,
		Sleep: sleeper.sleep,
	})
	require.NoError(t, err)
	t.Cleanup(m.stop)

	// initial login (1) + 7 scripted failures (2..8) + the recovering login (9)
	require.Eventually(t, func() bool { return auth.callCount() >= 9 },
		2*time.Second, time.Millisecond)

	delays := sleeper.recorded()
	require.GreaterOrEqual(t, len(delays), 8, "expected the lease-half wait plus 7 retry waits")

	retries := delays[1:8]
	for i, d := range retries {
		assert.LessOrEqualf(t, d, retryMaxWait, "retry %d (%s) exceeded retryMaxWait", i, d)
	}
	assert.Equal(t, retryMaxWait, retries[len(retries)-1], "backoff should have reached the cap by the 7th consecutive failure")
}

// A successful refresh must reset the backoff, so a later, unrelated outage
// starts retrying at retryInitialWait again rather than continuing from
// wherever the previous outage's backoff left off.
func TestTokenManager_SuccessResetsBackoff(t *testing.T) {
	ttl := 20 * time.Minute
	succ := scriptedResult{result: authResult{Token: "s.tok", TTL: ttl, Renewable: false}}
	fail := scriptedResult{err: errors.New("vault unreachable")}
	auth := &scriptedAuth{script: []scriptedResult{
		succ,             // 1: constructor login
		fail, fail, fail, // 2-4: first outage, 3 failures -> retryWait reaches 4m
		succ,       // 5: recovers
		fail, fail, // 6-7: second outage; the first of these must retry at retryInitialWait again
		succ, // 8: recovers, repeats thereafter
	}}
	sleeper := newRecordingSleep()
	m, err := newTokenManager(tokenManagerConfig{
		Auth: auth,
		Renew: func(context.Context, string) (time.Duration, error) {
			return 0, errors.New("unused: token is not renewable")
		},
		Now:   newTestClock().now,
		Sleep: sleeper.sleep,
	})
	require.NoError(t, err)
	t.Cleanup(m.stop)

	require.Eventually(t, func() bool { return auth.callCount() >= 8 },
		2*time.Second, time.Millisecond)

	delays := sleeper.recorded()
	require.GreaterOrEqual(t, len(delays), 6, "expected: lease wait, 3 retries, lease wait, first retry of the new outage")

	// delays: [0]=lease wait, [1..3]=1m,2m,4m (first outage), [4]=lease wait
	// after recovery, [5]=first retry of the second outage.
	assert.Equal(t, retryInitialWait, delays[5],
		"backoff must reset to retryInitialWait after a success, not continue from the prior outage's backoff")
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
