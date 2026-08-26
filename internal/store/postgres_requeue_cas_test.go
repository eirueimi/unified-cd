package store

import (
	"context"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requeueRunSpec is the smallest spec a claimable run needs.
const requeueRunSpec = `{"steps":[{"name":"s","run":"true"}]}`

// newClaimedRun creates a run, moves it Pending -> Queued, and has agentID
// claim it. It returns the run id, in the exact state RequeueClaimedRun is
// designed to reverse.
func newClaimedRun(t *testing.T, pg *Postgres, agentID string) string {
	t.Helper()
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(requeueRunSpec), nil, nil, "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)

	require.NoError(t, pg.UpsertAgent(ctx, agentID, "h", "linux", "dev", nil, nil, nil))
	claimed, err := pg.ClaimNextRun(ctx, agentID, nil)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, run.ID, claimed.ID)
	return run.ID
}

// The happy path: the agent that holds the claim gives it back, and the run
// returns to the queue in the state it was picked from — Queued, unclaimed,
// and genuinely claimable again.
func TestRequeueClaimedRun_ReturnsTheRunToTheQueue(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	runID := newClaimedRun(t, pg, "a1")

	requeued, err := pg.RequeueClaimedRun(ctx, runID, "a1")
	require.NoError(t, err)
	assert.True(t, requeued)

	got, err := pg.GetRun(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, api.RunQueued, got.Status)
	assert.Empty(t, got.ClaimedBy)

	// And it can be claimed again — by a different agent, since the point of
	// the requeue is that the run is back in the pool rather than pinned to
	// whoever released it.
	require.NoError(t, pg.UpsertAgent(ctx, "a2", "h", "linux", "dev", nil, nil, nil))
	again, err := pg.ClaimNextRun(ctx, "a2", nil)
	require.NoError(t, err)
	require.NotNil(t, again)
	assert.Equal(t, runID, again.ID)
}

// Both halves of the CAS, each pinned on its own.
//
// This test exists because deleting either half of
// `WHERE status = 'Running' AND claimed_by = $2` passed every other test on
// the branch. The interleavings that make each half load-bearing are safe
// today only because of who happens to write to runs.status; a CAS with
// neither half pinned is the shape that erodes silently as writers are added.
func TestRequeueClaimedRun_CASGuardsBothStatusAndOwner(t *testing.T) {
	ctx := context.Background()

	t.Run("claimed_by: another agent's claim is never taken away", func(t *testing.T) {
		pg := NewTestPostgres(t)
		runID := newClaimedRun(t, pg, "a1")

		// a2 asks to requeue a run a1 is executing. Without `AND claimed_by`,
		// this succeeds: the run goes back to Queued and is claimed a second
		// time while a1 is still running its steps, so one run executes twice.
		requeued, err := pg.RequeueClaimedRun(ctx, runID, "a2")
		require.NoError(t, err, "losing the CAS is an ordinary outcome, not an error")
		assert.False(t, requeued)

		got, err := pg.GetRun(ctx, runID)
		require.NoError(t, err)
		assert.Equal(t, api.RunRunning, got.Status, "the run must still belong to a1")
		assert.Equal(t, "a1", got.ClaimedBy)
	})

	t.Run("status: a run that already left Running is never resurrected", func(t *testing.T) {
		pg := NewTestPostgres(t)
		runID := newClaimedRun(t, pg, "a1")

		// The interim event the status half exists for: the run is cancelled
		// between the claim and the caller's decision to requeue. Without
		// `AND status = 'Running'`, the terminal run is dragged back to Queued
		// and re-executed after the user cancelled it.
		updated, err := pg.FinishRun(ctx, runID, api.RunCancelled)
		require.NoError(t, err)
		require.True(t, updated)

		requeued, err := pg.RequeueClaimedRun(ctx, runID, "a1")
		require.NoError(t, err)
		assert.False(t, requeued)

		got, err := pg.GetRun(ctx, runID)
		require.NoError(t, err)
		assert.Equal(t, api.RunCancelled, got.Status, "a cancelled run must stay cancelled")
	})
}
