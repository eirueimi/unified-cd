package store

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An approval decision is an audit record naming a human principal. On a run that
// has already reached a terminal state it is false on its face — and worse, when it
// lands inside the agent's cancel-detection window the gate returns true and the
// post-gate step body actually executes on a Cancelled run. The guard is in the SQL
// so it is atomic with the write: a handler-side status read would race the run's
// own terminalization landing between the read and the UPDATE.
func TestDecideApproval_RefusedOnceTheRunIsTerminal(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))

	for _, terminal := range []api.RunStatus{api.RunFailed, api.RunCancelled, api.RunSucceeded} {
		t.Run(string(terminal), func(t *testing.T) {
			run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
			require.NoError(t, err)
			future := time.Now().Add(time.Hour)
			require.NoError(t, pg.CreatePendingApproval(ctx, run.ID, 1, "gate", "ok?", &future))

			require.NoError(t, pg.MarkRunFinished(ctx, run.ID, terminal))

			changed, err := pg.DecideApproval(ctx, run.ID, 1, "Approved", "alice", "lgtm")
			require.NoError(t, err)
			assert.False(t, changed, "a terminal run must not accept an approval decision")

			// The audit row must be untouched — still Pending, no principal, no
			// decided_at — so nothing anywhere claims a human approved this run.
			got, err := pg.GetApproval(ctx, run.ID, 1)
			require.NoError(t, err)
			assert.Equal(t, "Pending", got.Status)
			assert.Empty(t, got.DecidedBy)
			assert.Nil(t, got.DecidedAt)
		})
	}
}

// Past timeout_at the agent has already failed the step locally, so a decision can
// no longer affect execution and only falsifies the record. (The approval reaper
// reconciles such rows to TimedOut/system within ~1 minute; this closes the window
// before it, which the measured incidents landed inside.)
func TestDecideApproval_RefusedOnceTheGateHasExpired(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)

	past := time.Now().Add(-time.Minute)
	require.NoError(t, pg.CreatePendingApproval(ctx, run.ID, 1, "gate", "ok?", &past))

	changed, err := pg.DecideApproval(ctx, run.ID, 1, "Approved", "alice", "")
	require.NoError(t, err)
	assert.False(t, changed)

	got, err := pg.GetApproval(ctx, run.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, "Pending", got.Status, "left for the approval reaper to mark TimedOut/system")
}

// The happy path is unaffected: a live run with an open gate still accepts the
// decision. Without this the guard could "pass" by refusing everything.
func TestDecideApproval_AcceptedWhileTheRunIsLiveAndTheGateIsOpen(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	_, err = pg.ClaimNextRun(ctx, "agent1", nil)
	require.NoError(t, err)

	future := time.Now().Add(time.Hour)
	require.NoError(t, pg.CreatePendingApproval(ctx, run.ID, 1, "gate", "ok?", &future))

	changed, err := pg.DecideApproval(ctx, run.ID, 1, "Approved", "alice", "lgtm")
	require.NoError(t, err)
	assert.True(t, changed)

	got, err := pg.GetApproval(ctx, run.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, "Approved", got.Status)
	assert.Equal(t, "alice", got.DecidedBy)
}

// A gate with no deadline at all (timeoutMinutes unset) must still be decidable.
func TestDecideApproval_NullTimeoutIsNotTreatedAsExpired(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	require.NoError(t, pg.CreatePendingApproval(ctx, run.ID, 1, "gate", "ok?", nil))

	changed, err := pg.DecideApproval(ctx, run.ID, 1, "Approved", "alice", "")
	require.NoError(t, err)
	assert.True(t, changed)
}
