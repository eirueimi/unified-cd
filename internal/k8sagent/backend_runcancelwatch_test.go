package k8sagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// startRunCancelWatch (backend.go) had no test coverage at all: every other
// k8sagent test drives a backend with b.a.client == nil, which is exactly the
// guard the watch bails out on, so the goroutine has never actually run under
// test. These tests drive the REAL watch against a real *agentlib.Client and
// httptest.Server (the same pattern orchestrate_cancel_test.go uses for the
// standard agent's cancel poller), with a fake podManager standing in for the
// cluster so scope-pod creation itself is deterministic.

// runCancelWatchHarness builds a k8sBackend whose b.a.client talks to an
// httptest.Server serving GET /api/v1/runs/{id} via getRun, and whose
// b.a.pm is pm. Poll intervals are shortened for the duration of the test.
func runCancelWatchHarness(t *testing.T, pm *fakePM, getRun http.HandlerFunc) *k8sBackend {
	t.Helper()
	prev := agentlib.CancelPollInterval
	agentlib.CancelPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { agentlib.CancelPollInterval = prev })

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/runs/{id}", getRun)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := agentlib.NewClient(srv.URL, "tok")
	a := &K8sAgent{cfg: Config{Namespace: "default", PodStartTimeout: "5m"}, pm: pm, client: client}
	return newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})
}

// TestK8sBackend_StartRunCancelWatch_AbortsScopeCreationOnTerminalStatus
// proves the watch's actual job: once the controller reports the claim's run
// terminal, an in-flight scope-pod creation must be aborted promptly rather
// than sit out the full PodStartTimeout (5 minutes here, and the fake's
// WaitForPodRunning is never released — if the watch did not fire, this test
// would hang until its own 10s bound and fail).
func TestK8sBackend_StartRunCancelWatch_AbortsScopeCreationOnTerminalStatus(t *testing.T) {
	pm := &fakePM{
		blockFirstWait: make(chan struct{}), // deliberately never closed
		waitStarted:    make(chan struct{}),
	}
	b := runCancelWatchHarness(t, pm, func(w http.ResponseWriter, r *http.Request) {
		orchestrateWriteJSON(w, api.Run{ID: "run-1", Status: api.RunCancelled})
	})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := b.EnsureScope(context.Background(), step, nil)
		done <- err
	}()

	<-pm.waitStarted // creation is genuinely in flight, parked in the pod wait

	select {
	case err := <-done:
		require.Error(t, err, "a terminal run status at the controller must abort scope pod creation")
		assert.Less(t, time.Since(start), 10*time.Second,
			"the abort must arrive within a few poll intervals, not after PodStartTimeout")
	case <-time.After(10 * time.Second):
		t.Fatal("scope pod creation did not abort: the run-cancel watch reached no abort path")
	}

	assert.Equal(t, 1, pm.creations())
	pm.mu.Lock()
	assert.Len(t, pm.deleted, 1, "the aborted creation's Pod must still be cleaned up")
	pm.mu.Unlock()
}

// TestK8sBackend_StartRunCancelWatch_NonTerminalStatusDoesNotAbort is the
// control: a live, non-terminal status must never trip the abort path. Kept
// short and paired with the transport-error/non-200 tests below so the
// "does this poll response abort creation" matrix has an explicit false
// case, not just an absence of failure.
func TestK8sBackend_StartRunCancelWatch_NonTerminalStatusDoesNotAbort(t *testing.T) {
	pm := &fakePM{}
	b := runCancelWatchHarness(t, pm, func(w http.ResponseWriter, r *http.Request) {
		orchestrateWriteJSON(w, api.Run{ID: "run-1", Status: api.RunRunning})
	})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}
	_, err := b.EnsureScope(context.Background(), step, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, pm.creations())
	pm.mu.Lock()
	assert.Empty(t, pm.deleted)
	pm.mu.Unlock()
}

// TestK8sBackend_StartRunCancelWatch_NonOKStatusDoesNotAbort pins the failure
// mode the review specifically flagged: treating "the controller answered but
// not with 200" as "the run is terminal" would abort healthy scope creations
// on a controller-side hiccup (a 500, a restart mid-request) that has nothing
// to do with the run's actual status. agentlib.Client.GetRun turns any
// non-2xx response into an *agentlib.HTTPError, so the watch must treat that
// exactly like any other GetRun error: log and keep polling, never call
// b.scopeCancel.
func TestK8sBackend_StartRunCancelWatch_NonOKStatusDoesNotAbort(t *testing.T) {
	var calls int32
	pm := &fakePM{
		blockFirstWait: make(chan struct{}),
		waitStarted:    make(chan struct{}),
	}
	b := runCancelWatchHarness(t, pm, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "internal error", http.StatusInternalServerError)
	})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}
	done := make(chan error, 1)
	go func() {
		_, err := b.EnsureScope(context.Background(), step, nil)
		done <- err
	}()

	<-pm.waitStarted

	// Let several poll ticks land, every one of them a 500, while creation is
	// still parked — the exact window a wrongly-terminal read would abort in.
	require.Eventually(t, func() bool { return atomic.LoadInt32(&calls) >= 3 },
		time.Second, time.Millisecond, "the watch must keep polling through repeated non-200 responses")

	close(pm.blockFirstWait)

	select {
	case err := <-done:
		require.NoError(t, err, "repeated non-200 responses from the controller must not abort scope pod creation")
	case <-time.After(5 * time.Second):
		t.Fatal("scope pod creation did not complete")
	}
	assert.Equal(t, 1, pm.creations())
	pm.mu.Lock()
	assert.Empty(t, pm.deleted, "a healthy creation must not be torn down because the controller was briefly unreachable")
	pm.mu.Unlock()
}

// TestK8sBackend_StartRunCancelWatch_TransportErrorDoesNotAbort is the other
// half: a connection-level failure (the controller unreachable, not merely
// answering with an error status) must be equally harmless. The server is
// closed before the client ever talks to it, so every GetRun call fails at
// the transport layer rather than returning any HTTP response at all.
func TestK8sBackend_StartRunCancelWatch_TransportErrorDoesNotAbort(t *testing.T) {
	prev := agentlib.CancelPollInterval
	agentlib.CancelPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { agentlib.CancelPollInterval = prev })

	srv := httptest.NewServer(http.NewServeMux())
	srv.Close() // closed immediately: every request now fails at the transport, not with an HTTP status

	client := agentlib.NewClient(srv.URL, "tok")
	pm := &fakePM{
		blockFirstWait: make(chan struct{}),
		waitStarted:    make(chan struct{}),
	}
	a := &K8sAgent{cfg: Config{Namespace: "default", PodStartTimeout: "5m"}, pm: pm, client: client}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}
	done := make(chan error, 1)
	go func() {
		_, err := b.EnsureScope(context.Background(), step, nil)
		done <- err
	}()

	<-pm.waitStarted

	// Hold creation in flight through several poll intervals, every one of
	// them a connection failure, before letting it succeed.
	time.Sleep(15 * agentlib.CancelPollInterval)
	close(pm.blockFirstWait)

	select {
	case err := <-done:
		require.NoError(t, err, "a controller the agent simply cannot reach must not abort scope pod creation")
	case <-time.After(5 * time.Second):
		t.Fatal("scope pod creation did not complete")
	}
	assert.Equal(t, 1, pm.creations())
	pm.mu.Lock()
	assert.Empty(t, pm.deleted, "a healthy creation must not be torn down because the controller is unreachable")
	pm.mu.Unlock()
}

// TestK8sBackend_CloseScopes_StopsRunCancelWatch pins D3: the watch is
// cancelled by CloseScopes but was never joined, unlike RunClaim's own cancel
// poller, which it deliberately mirrors and which does `cancelRun();
// pollerWG.Wait()`. The watch already exits promptly on ctx.Done(), so this
// cannot expose a stuck goroutine; what it pins is that CloseScopes does not
// return while the watch could still be about to touch claim state (here,
// the controller) — checked by confirming no further polls land once
// CloseScopes has returned, given a generous grace period.
//
// This test flaked on main (CI #33039380133): "expected 2, actual 3", the
// extra call landing during the grace sleep, apparently after CloseScopes
// had already returned. Instrumenting it (a scratch build logging each
// handler call's timestamp, remote address, and CloseScopes' own
// call/return times) pinned the mechanism precisely: the extra call always
// comes from the SAME client connection (identical remote port) as the
// earlier ones — it is the real watch loop, not a leaked goroutine or a
// cross-test/cross-iteration collision — and it lands within a few hundred
// microseconds to low milliseconds of CloseScopes returning, sometimes at
// the same logged instant.
//
// The mechanism: the watch's ticker fires on its own ~5ms cadence,
// independent of CloseScopes. When a tick and b.scopeCancel() (which
// CloseScopes calls immediately, before anything else) land close enough
// together, the watch loop's `select` can still choose the ticker.C branch
// — Go does not prioritize select cases by order, and watchCtx.Done()
// becoming ready does not retroactively un-choose an already-scheduled tick.
// The resulting GetRun request goes out over the watch's already-open,
// reused connection, and a write to an established connection can complete
// (bytes handed to the OS) faster than the goroutine can also notice
// ctx.Done() and abort first. That request is genuinely in flight — sent —
// before CloseScopes returns; only the SERVER's dispatch of the handler that
// counts it can lag behind, especially under load (both CI failures
// happened on a busy machine), landing after CloseScopes has already
// returned even though the request was ALREADY on the wire before it did.
// Critically, this can happen AT MOST ONCE per CloseScopes call: after that
// one race-losing tick, the very next loop iteration finds watchCtx.Done()
// already ready with no tick simultaneously ready (the next one is a full
// poll interval away), so it returns cleanly.
//
// This is not evidence the watch goroutine kept running past the join —
// CloseScopes' own join (runCancelWatchWG.Wait, raced against ctx.Done)
// still guarantees the goroutine has executed its `return` by the time
// CloseScopes returns, and does not send another GetRun after that. It is a
// single already-committed straggler, indistinguishable in principle from
// any other network request whose completion timing the sender does not
// control. Snapshotting `calls` the instant CloseScopes returns can race
// that straggler's landing; debouncing — waiting until calls has gone quiet
// for a full settle window before trusting it as the baseline — waits it
// out, while still catching a watch that keeps polling for a genuinely bad
// reason (that would keep calls changing well past one settle window).
func TestK8sBackend_CloseScopes_StopsRunCancelWatch(t *testing.T) {
	var calls int32
	pm := &fakePM{}
	b := runCancelWatchHarness(t, pm, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		orchestrateWriteJSON(w, api.Run{ID: "run-1", Status: api.RunRunning})
	})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}
	_, err := b.EnsureScope(context.Background(), step, nil)
	require.NoError(t, err)

	require.Eventually(t, func() bool { return atomic.LoadInt32(&calls) >= 2 },
		time.Second, time.Millisecond, "the watch must have started polling")

	b.CloseScopes(context.Background())

	// See the doc comment above: at most one already-committed straggler tick
	// can still land after CloseScopes returns, with no bearing on whether
	// the watch is still running. Debounce it out — wait until calls has
	// stopped changing for a full settle window (comfortably many poll
	// intervals) — before trusting its value as the baseline, rather than
	// reading it the instant CloseScopes returns.
	const settleWindow = 100 * time.Millisecond
	lastSeen, stableSince := int32(-1), time.Time{}
	require.Eventually(t, func() bool {
		cur := atomic.LoadInt32(&calls)
		if cur != lastSeen {
			lastSeen, stableSince = cur, time.Now()
			return false
		}
		return time.Since(stableSince) >= settleWindow
	}, 2*time.Second, time.Millisecond, "calls never stopped changing after CloseScopes returned")
	afterClose := atomic.LoadInt32(&calls)

	time.Sleep(50 * time.Millisecond) // many poll intervals' worth of grace
	assert.Equal(t, afterClose, atomic.LoadInt32(&calls),
		"no poll may land after CloseScopes returns: the watch must already be stopped, not merely asked to stop")
}
