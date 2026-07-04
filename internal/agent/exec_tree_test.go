package agent

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunTreeKilled_NormalCompletion_ReleasesJobHandle is a regression test
// for TODO #22: the #9c process-tree-kill work assigned a Windows Job Object
// per step (assignJob, in exec_windows.go) but only released it on the
// cancel path (killTree). A step that completes normally never freed the
// handle, so jobHandles grew by one entry (and one leaked kernel Job Object
// handle) per step, unboundedly, on a long-running Windows agent.
//
// This test drives runTreeKilled through a normal (non-cancelled) exit and
// asserts jobHandleCount() is back to 0 afterward. On Unix, jobHandleCount
// always returns 0 (there is no jobHandles map there), so the assertion
// still holds — it's the cross-platform invariant cleanupTree is meant to
// guarantee. On Windows, this exercises the real fix: without it, this test
// fails because jobHandleCount() stays at 1.
func TestRunTreeKilled_NormalCompletion_ReleasesJobHandle(t *testing.T) {
	before := jobHandleCount()

	cmd := exec.Command(findShell(), "-lc", "exit 0")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := runTreeKilled(ctx, cmd)
	require.NoError(t, err, "normally-completing step should not error")

	assert.Equal(t, before, jobHandleCount(),
		"jobHandles must not grow after a normally-completed step (TODO #22 leak regression)")
}

// TestRunTreeKilled_MultipleNormalCompletions_NoAccumulation runs several
// steps back to back and asserts the handle count never grows, guarding
// against the "leaks unboundedly on a long-running agent" failure mode
// described in TODO #22 (as opposed to a single leaked handle that might be
// dismissed as harmless).
func TestRunTreeKilled_MultipleNormalCompletions_NoAccumulation(t *testing.T) {
	before := jobHandleCount()

	for i := 0; i < 5; i++ {
		cmd := exec.Command(findShell(), "-lc", "exit 0")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := runTreeKilled(ctx, cmd)
		cancel()
		require.NoError(t, err)
		assert.Equal(t, before, jobHandleCount(),
			"jobHandles must return to baseline after each normally-completed step (iteration %d)", i)
	}
}

// TestRunTreeKilled_Cancel_StillReleasesJobHandle guards the other exit
// path: cancellation via killTree must still result in the handle being
// released exactly once (not double-closed, not leaked), now that cleanup
// also runs unconditionally via defer in runTreeKilled. takeJobHandle's
// delete-then-check semantics mean killTree "wins" the handle here and the
// deferred cleanupTree becomes a no-op — this test confirms that combination
// doesn't leak or panic (e.g. from double CloseHandle).
func TestRunTreeKilled_Cancel_StillReleasesJobHandle(t *testing.T) {
	before := jobHandleCount()

	cmd := exec.Command(findShell(), "-lc", "sleep 30")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- runTreeKilled(ctx, cmd)
	}()

	// Give the process a moment to actually start before cancelling.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.Error(t, err, "cancelled step should return ctx.Err()")
	case <-time.After(15 * time.Second):
		t.Fatal("runTreeKilled did not return within 15s of cancellation")
	}

	assert.Equal(t, before, jobHandleCount(),
		"jobHandles must not leak on the cancel path either")
}
