package k8sagent

import (
	"context"
	"sync"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestK8sBackend_EnsureScope_SameKeyCreatesOnePod drives EnsureScope
// concurrently for one scope key, which is what a parallel: group or a matrix
// whose members share a uses-scope does once the backend reports Concurrent.
//
// Two properties are asserted, and they fail for different reasons:
//
//   - Under -race, the old map[string]string version reports a data race on
//     b.scopePods — concurrent read and write of a plain map.
//   - Even without -race, the old check-then-act let both goroutines miss the
//     cache and each create a pod. One won the map entry; the other's pod was
//     orphaned, because CloseScopes only deletes pods that made it into the
//     map. The creation count is what catches that.
func TestK8sBackend_EnsureScope_SameKeyCreatesOnePod(t *testing.T) {
	pm := &fakePM{}
	a := &K8sAgent{cfg: Config{Namespace: "default"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}

	const goroutines = 8
	var wg sync.WaitGroup
	payloads := make([]any, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})

	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			h, err := b.EnsureScope(context.Background(), step, nil)
			payloads[i], _ = agentlib.ScopeHandlePayload(h)
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range goroutines {
		require.NoError(t, errs[i], "goroutine %d", i)
	}
	assert.Equal(t, 1, pm.creations(), "one scope key must produce exactly one pod, however many steps ask for it at once")
	for i := range goroutines {
		assert.Equal(t, payloads[0], payloads[i], "every caller for one key must get the same scope pod")
	}
}

// TestK8sBackend_EnsureScope_DifferentKeysDoNotSerialize proves the in-flight
// entry is per key rather than one lock around the whole function. The fake
// blocks the first WaitForPodRunning until released; if scope creation were
// serialised under a single mutex, the second key's request would block behind
// it and this test would deadlock until the test timeout.
func TestK8sBackend_EnsureScope_DifferentKeysDoNotSerialize(t *testing.T) {
	pm := &fakePM{
		blockFirstWait: make(chan struct{}),
		waitStarted:    make(chan struct{}),
	}
	a := &K8sAgent{cfg: Config{Namespace: "default"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	first := make(chan struct{})
	go func() {
		defer close(first)
		_, _ = b.EnsureScope(context.Background(), api.ClaimStep{ScopeID: "scope:a", ScopeImage: "golang:1.22"}, nil)
	}()

	// Wait until the first key is definitely parked in its pod wait, so the
	// second key's request is unambiguously concurrent with it.
	<-pm.waitStarted

	second := make(chan error, 1)
	go func() {
		_, err := b.EnsureScope(context.Background(), api.ClaimStep{ScopeID: "scope:b", ScopeImage: "node:22"}, nil)
		second <- err
	}()

	// Under a single lock held across creation, the second key would block
	// acquiring it and this would time out instead.
	require.NoError(t, <-second, "a second scope key must not wait on the first key's pod")

	close(pm.blockFirstWait)
	<-first
}

// TestK8sBackend_EnsureScope_ShortDeadlineCallerDoesNotPoisonHealthyCaller
// pins the fix for the defect concurrent step execution introduced: the
// winner of scopeEntry.once creates the scope pod for EVERY caller of that
// key, so if creation ran under the winner's own step context, a member with
// `timeout: 1` would abort a slow image pull, delete the Pod, and hand its
// own DeadlineExceeded to a sibling whose context was perfectly healthy.
// Impossible under Sequential (one caller existed at a time, and a failure is
// deliberately not cached, so the next caller made its own attempt), and not
// how the host behaves either — scopeManager.ensure (internal/agent/scope.go)
// caches only successes, so its second caller retries under its own context.
//
// The fake parks the first WaitForPodRunning until released, standing in for
// a scope image that takes longer to pull than one member's timeout. The
// short-deadline caller wins the Once (the test waits for pm.waitStarted
// before starting the healthy one), then its deadline expires while the pull
// is still in flight. The decisive assertions are on the HEALTHY caller: it
// must still get a pod, and no pod may have been deleted out from under it.
//
// Against the pre-fix code this fails on every assertion at once — the wait
// returns the expired caller's context error, createScopePod deletes the Pod,
// and both callers receive that error.
func TestK8sBackend_EnsureScope_ShortDeadlineCallerDoesNotPoisonHealthyCaller(t *testing.T) {
	pm := &fakePM{
		blockFirstWait: make(chan struct{}),
		waitStarted:    make(chan struct{}),
	}
	a := &K8sAgent{cfg: Config{Namespace: "default", PodStartTimeout: "30s"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}

	// The impatient member: a step-level `timeout:` expressed the way the
	// orchestrator expresses it, as a deadline on the context it hands
	// EnsureScope.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer shortCancel()

	shortDone := make(chan error, 1)
	go func() {
		_, err := b.EnsureScope(shortCtx, step, nil)
		shortDone <- err
	}()

	// Do not start the healthy caller until the impatient one is definitely
	// the winner of the Once and parked inside the pod wait; otherwise the
	// test would race on which member owns the attempt.
	<-pm.waitStarted

	healthyDone := make(chan struct {
		name string
		err  error
	}, 1)
	go func() {
		h, err := b.EnsureScope(context.Background(), step, nil)
		payload, _ := agentlib.ScopeHandlePayload(h)
		name, _ := payload.(string)
		healthyDone <- struct {
			name string
			err  error
		}{name, err}
	}()

	// Let the impatient caller's deadline actually expire while the pull is
	// still in flight. This is the exact window the defect lived in.
	<-shortCtx.Done()
	require.ErrorIs(t, shortCtx.Err(), context.DeadlineExceeded)

	// Nothing may have been torn down on that caller's behalf: the creation
	// belongs to the key, not to whoever asked for it first.
	pm.mu.Lock()
	deletedSoFar := len(pm.deleted)
	pm.mu.Unlock()
	assert.Zero(t, deletedSoFar, "the expired caller's deadline must not delete the shared scope pod")

	// The pull finally completes, after the impatient caller is long gone.
	close(pm.blockFirstWait)

	healthy := <-healthyDone
	require.NoError(t, healthy.err, "the healthy caller must not inherit the other member's step timeout")
	require.NotEmpty(t, healthy.name, "the healthy caller must still get a scope pod")

	require.NoError(t, <-shortDone, "the shared attempt succeeded, so every caller sees the success")

	assert.Equal(t, 1, pm.creations(), "one scope key must still produce exactly one pod")
	pm.mu.Lock()
	assert.Empty(t, pm.deleted, "no scope pod may be deleted while a live caller still holds it")
	pm.mu.Unlock()
}

// TestK8sBackend_EnsureScope_CallerCancellationAbortsCreation is the other
// half of the contract above: creation must ignore a caller's DEADLINE (that
// is one step's private `timeout:`) but must still honour a caller's
// CANCELLATION, which on a step context propagates from RunClaim's runCtx and
// therefore means the run itself is over — cancelled at the controller, or the
// agent shutting down. Without this, a cancelled run would sit waiting on an
// image pull for the full PodStartTimeout.
func TestK8sBackend_EnsureScope_CallerCancellationAbortsCreation(t *testing.T) {
	pm := &fakePM{
		blockFirstWait: make(chan struct{}),
		waitStarted:    make(chan struct{}),
	}
	a := &K8sAgent{cfg: Config{Namespace: "default", PodStartTimeout: "5m"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := b.EnsureScope(ctx, step, nil)
		done <- err
	}()

	<-pm.waitStarted
	cancel()

	// blockFirstWait is never closed: if cancellation did not reach the
	// creation context, this would block until the test times out rather than
	// returning, because PodStartTimeout is 5 minutes.
	err := <-done
	require.Error(t, err, "a cancelled run must abort scope pod creation rather than wait out PodStartTimeout")
	assert.ErrorIs(t, err, context.Canceled)
}

// TestK8sBackend_EnsureScope_FailureIsNotCached proves a failed creation does
// not poison the key for the rest of the claim: the entry is dropped so a
// later step retries, rather than inheriting an error it did not cause. Scope
// pods fail for reasons that are frequently transient — image pull, quota, a
// node filling up.
func TestK8sBackend_EnsureScope_FailureIsNotCached(t *testing.T) {
	pm := &fakePM{waitErr: assert.AnError}
	a := &K8sAgent{cfg: Config{Namespace: "default"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}

	_, err := b.EnsureScope(context.Background(), step, nil)
	require.Error(t, err, "the fake's wait error must surface")

	pm.mu.Lock()
	pm.waitErr = nil
	pm.mu.Unlock()

	_, err = b.EnsureScope(context.Background(), step, nil)
	require.NoError(t, err, "a later step must get its own attempt, not the cached failure")
	assert.Equal(t, 2, pm.creations(), "the retry must actually create a pod rather than return a cached entry")
}

// TestK8sBackend_CloseScopes_DoesNotDoubleDeleteRacingFailedCreate drives
// CloseScopes concurrently with an in-flight ensureScopePod attempt that is
// about to fail its WaitForPodRunning, which is exactly the ownership race
// described at CloseScopes and in createScopePod's failure branch: e.name is
// recorded before e.err is known, so a CloseScopes that reads the entry in
// that window sees e.err == nil (not yet set) and e.name != "" and would,
// without the ownership handoff, issue its own DeletePod for the same pod
// createScopePod's own failure branch also deletes.
//
// In production this cannot actually overlap (CloseScopes is deferred until
// after RunPipeline returns and runParallel has joined), so this test drives
// the two calls directly against the backend, bypassing that ordering, to
// pin the handoff itself rather than the orchestrator's scheduling. It also
// doubles as a race check on the ownership logic: fakePM.DeletePod appends to
// a slice without its own lock, so two concurrent deletes would be flagged by
// -race even before the assertions below run.
func TestK8sBackend_CloseScopes_DoesNotDoubleDeleteRacingFailedCreate(t *testing.T) {
	pm := &fakePM{
		waitErr:        assert.AnError,
		blockFirstWait: make(chan struct{}),
		waitStarted:    make(chan struct{}),
	}
	a := &K8sAgent{cfg: Config{Namespace: "default"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}

	ensureDone := make(chan error, 1)
	go func() {
		_, err := b.EnsureScope(context.Background(), step, nil)
		ensureDone <- err
	}()

	// Wait until the in-flight attempt has recorded e.name and is parked in
	// WaitForPodRunning, so CloseScopes below is unambiguously racing a
	// window where e.err is still unset.
	<-pm.waitStarted

	// CloseScopes claims the entry (e.err == nil, e.name != "") and deletes
	// its pod before the in-flight attempt ever learns it failed.
	b.CloseScopes(context.Background())

	// Now let WaitForPodRunning return the failure. createScopePod's ownership
	// check must find its key no longer maps to its own entry and skip its
	// own delete.
	close(pm.blockFirstWait)
	require.Error(t, <-ensureDone, "the fake's wait error must still surface to the caller")

	assert.Equal(t, 1, pm.creations(), "exactly one scope pod should have been created")
	assert.Len(t, pm.deleted, 1, "the pod must be deleted exactly once, not once by CloseScopes and again by createScopePod's failure branch")
	if len(pm.deleted) > 0 {
		assert.Equal(t, pm.createdNm, pm.deleted[0], "the deleted pod must be the one that was created")
	}
}

// TestK8sBackend_EnsureScopePod_DoesNotEvictLiveSuccessorOnFailure pins the
// leak Finding 1 identified: ensureScopePod's once.Do closure used to run an
// UNCONDITIONAL delete(b.scopes, key) on a failed attempt. createScopePod's
// own failure branch already removes its own entry (and releases scopesMu)
// before calling DeletePod, so there is a real window — between that delete
// and the once.Do closure re-acquiring scopesMu — during which a later
// caller for the SAME key can see the key is free, install a brand new
// entry, and succeed. The old unconditional delete would then evict that
// live, successful entry instead of the stale failed one, and its pod would
// never be found (and so never deleted) by CloseScopes: a leak, not merely a
// redundant API call. This is a logic race, not a data race — every access
// here is already under scopesMu — so -race cannot find it; only a fake that
// can hold a goroutine open across that specific window can.
//
// The fake widens that window deterministically: fakePM.blockFirstDelete
// parks the first (failing) attempt's DeletePod call open until the test
// releases it, and fakePM.failWaitCalls makes only that first
// WaitForPodRunning call fail, so a second attempt started while the first
// is parked succeeds cleanly and installs a live entry for the same key.
func TestK8sBackend_EnsureScopePod_DoesNotEvictLiveSuccessorOnFailure(t *testing.T) {
	pm := &fakePM{
		waitErr:          assert.AnError,
		failWaitCalls:    1, // only the first attempt's WaitForPodRunning fails
		blockFirstDelete: make(chan struct{}),
		deleteStarted:    make(chan struct{}),
	}
	a := &K8sAgent{cfg: Config{Namespace: "default"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}

	// The first attempt: fails WaitForPodRunning, removes its own entry
	// (ownership check in createScopePod's failure branch), then parks
	// inside DeletePod.
	firstDone := make(chan error, 1)
	go func() {
		_, err := b.EnsureScope(context.Background(), step, nil)
		firstDone <- err
	}()
	<-pm.deleteStarted // the key is now free; the first attempt is parked

	// The second attempt, for the SAME key, started while the first is still
	// parked inside DeletePod. WaitForPodRunning is call #2 overall, which
	// failWaitCalls: 1 lets succeed, so this installs a live entry.
	type secondResult struct {
		name string
		err  error
	}
	secondDone := make(chan secondResult, 1)
	go func() {
		h, err := b.EnsureScope(context.Background(), step, nil)
		payload, _ := agentlib.ScopeHandlePayload(h)
		name, _ := payload.(string)
		secondDone <- secondResult{name: name, err: err}
	}()

	second := <-secondDone
	require.NoError(t, second.err, "the second attempt, for a key the first attempt had already freed, must succeed on its own merits")
	require.NotEmpty(t, second.name)

	// Now release the first attempt. Its once.Do closure re-acquires
	// scopesMu and must find the key no longer maps to its own (stale)
	// entry — the second attempt's live entry is there instead — and must
	// NOT delete it.
	close(pm.blockFirstDelete)
	require.Error(t, <-firstDone, "the first attempt's own WaitForPodRunning failure must still surface to its caller")

	assert.Equal(t, 2, pm.creations(), "both attempts should have created a pod")

	// The decisive assertion: the second attempt's pod must still be
	// reachable by CloseScopes. Before the fix, the first attempt's closure
	// evicted the second attempt's entry from b.scopes, so CloseScopes would
	// never see it and this pod would leak forever.
	b.CloseScopes(context.Background())
	assert.Contains(t, pm.deleted, second.name, "the live successor's pod must still be deleted by CloseScopes, not orphaned by the first attempt's failure cleanup")
}
