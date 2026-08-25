package k8sagent

import (
	"context"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// scopeAbandonedForTest reports whether key's current scope entry has been
// abandoned (its interest reached zero), or false if there is no entry.
// Mirrors scopeInterestForTest (backend_scope_race_test.go).
func (b *k8sBackend) scopeAbandonedForTest(key string) bool {
	b.scopesMu.Lock()
	defer b.scopesMu.Unlock()
	if e, ok := b.scopes[key]; ok {
		return e.abandonedClosed
	}
	return false
}

// TestK8sBackend_EnsureScope_JoinDuringAbandonmentDoesNotInheritCancellation
// pins finding D1 from the concurrent-steps review: ensureScopePod's map
// lookup joined whatever entry it found for a key without checking whether
// that entry had already been abandoned (every earlier caller gone) while
// its creation attempt was still running, unfinished. A caller arriving in
// that window blocked in the shared sync.Once and inherited the abandoned
// attempt's failure, despite its own context being perfectly healthy.
//
// Scenario, matching the one in the review: two parallel: members share
// `uses: S`. Member A (modeled here as the winner, ctxA) wins the Once and
// is "pulling the image" (parked in WaitForPodRunning). A's own `timeout:`
// fires — A is the SOLE registered caller for this key, so the entry is
// abandoned. Member B (ctxB, never cancelled) then makes its first call for
// the same key, arriving squarely inside the unwind.
//
// The window this needs to land in is real but not microseconds-in-a-test
// reachable by accident: the REAL PodManager.WaitForPodRunning only notices
// ctx.Done() between 500ms polls (podmanager.go:108-113), which is exactly
// why the production window is "up to ~500ms" rather than instant. The fake
// reproduces that shape via ignoreCtxUntilGate: A's WaitForPodRunning call
// blocks purely on a gate the test controls, ignoring cancellation entirely
// until the test releases it — so the abandonment-but-not-yet-finished state
// can be held open for as long as needed, deterministically, instead of
// racing real goroutine scheduling.
func TestK8sBackend_EnsureScope_JoinDuringAbandonmentDoesNotInheritCancellation(t *testing.T) {
	pm := &fakePM{
		waitErr:            assert.AnError, // what A's attempt ultimately reports, once released
		failWaitCalls:      1,              // only A's (call #1) WaitForPodRunning fails; a later, fresh attempt succeeds
		blockFirstWait:     make(chan struct{}),
		waitStarted:        make(chan struct{}),
		ignoreCtxUntilGate: true,
	}
	a := &K8sAgent{cfg: Config{Namespace: "default", PodStartTimeout: "5m"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}
	key := scopeKey(step)

	// A: the winner, with its own cancellable context standing in for a
	// step's `timeout:` firing.
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	doneA := make(chan error, 1)
	go func() {
		_, err := b.EnsureScope(ctxA, step, nil)
		doneA <- err
	}()

	<-pm.waitStarted // A is parked in WaitForPodRunning, ignoring ctx.Done() until the gate opens

	// A's deadline fires. A is the sole registered caller, so this must
	// abandon the entry — while A's own attempt is still in flight (blocked
	// on the gate, its once.Do closure nowhere near finished).
	cancelA()
	require.Eventually(t, func() bool { return b.scopeAbandonedForTest(key) },
		time.Second, time.Millisecond, "A's cancellation must abandon the entry (interest reaches zero)")

	// B: a fresh caller for the SAME key, with a context that is never
	// cancelled — standing in for a sibling still on a healthy deadline. It
	// must not block behind, or inherit the result of, A's doomed attempt.
	ctxB := context.Background()
	type result struct {
		name string
		err  error
	}
	doneB := make(chan result, 1)
	go func() {
		h, err := b.EnsureScope(ctxB, step, nil)
		payload, _ := agentlib.ScopeHandlePayload(h)
		name, _ := payload.(string)
		doneB <- result{name, err}
	}()

	// Synchronize on B having actually reached this key: on the buggy code B
	// joins A's entry and blocks in once.Do (interest becomes visible); on
	// the fixed code B installs and runs its own fresh entry, which — being
	// unblocked — may complete before this check ever runs, so also accept
	// "a second pod was created" as proof B got there.
	require.Eventually(t, func() bool { return b.scopeInterestForTest(key) >= 1 || pm.creations() >= 2 },
		time.Second, time.Millisecond, "B must have reached the shared key, one way or the other")

	// Let A's attempt notice its cancellation and finish, the way a real next
	// poll tick would.
	close(pm.blockFirstWait)

	errA := <-doneA
	require.Error(t, errA, "A's own attempt must still fail: its context ended")

	resultB := <-doneB
	require.NoError(t, resultB.err, "B's healthy context must not be sacrificed to A's abandoned attempt")
	require.NotEmpty(t, resultB.name, "B must still get a scope pod")

	assert.Equal(t, 2, pm.creations(), "A's abandoned attempt and B's own attempt each create one pod")
	pm.mu.Lock()
	assert.Len(t, pm.deleted, 1, "only A's abandoned pod is cleaned up here; B's live pod is CloseScopes' job")
	assert.NotContains(t, pm.deleted, resultB.name, "B's own pod must not be the one that was deleted")
	pm.mu.Unlock()
}
