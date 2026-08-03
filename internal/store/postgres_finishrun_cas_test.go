package store

import (
	"context"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mutexHolder returns the run currently holding mutexName, or "" if it is free.
func mutexHolder(t *testing.T, pg *Postgres, mutexName string) string {
	t.Helper()
	var runID string
	err := pg.pool.QueryRow(context.Background(),
		`SELECT COALESCE(run_id::text, '') FROM mutex_holders WHERE mutex_name = $1`, mutexName).Scan(&runID)
	if err != nil {
		return ""
	}
	return runID
}

const mutexJobSpec = `{"concurrency":{"mutex":"m1"},"steps":[{"name":"s","run":"true"}]}`

// FinishRun's guard refuses only TERMINAL statuses, so a caller acting on a stale
// snapshot could terminalize a run that had since been claimed and started.
// FinishRunIfStatus adds the CAS the queued-run reaper needs: the write lands only
// while the run is still in the status the decision was made from.
func TestFinishRunIfStatus_RefusesWhenTheRunHasMovedOn(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)

	// A live agent claims it — the exact event that falsifies the reaper's
	// "no live agent can claim this" premise, 8ms after the snapshot in the
	// measured incident.
	require.NoError(t, pg.UpsertAgent(ctx, "agent1", "h", "linux", "dev", nil, nil, nil))
	claimed, err := pg.ClaimNextRun(ctx, "agent1", nil)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, run.ID, claimed.ID)

	updated, err := pg.FinishRunIfStatus(ctx, run.ID, api.RunQueued, api.RunFailed)
	require.NoError(t, err, "losing the race is an ordinary outcome, not an error")
	assert.False(t, updated)

	got, err := pg.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunRunning, got.Status, "the claimed, executing run must be left alone")

	// The plain FinishRun path is unchanged: it still terminalizes a Running run
	// (that is the agent's own finish report).
	updated, err = pg.FinishRun(ctx, run.ID, api.RunSucceeded)
	require.NoError(t, err)
	assert.True(t, updated)
}

func TestFinishRunIfStatus_TerminalizesWhenTheStatusStillMatches(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)

	updated, err := pg.FinishRunIfStatus(ctx, run.ID, api.RunQueued, api.RunFailed)
	require.NoError(t, err)
	assert.True(t, updated)

	got, err := pg.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunFailed, got.Status)
}

// The larger half of the same race: the lock releases used to be issued
// unconditionally, so a caller whose terminal write was refused still freed the
// run's mutex — out from under a live holder that is still executing inside it.
func TestFinishRunIfStatus_DoesNotReleaseLocksItDidNotWin(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(mutexJobSpec))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(mutexJobSpec), nil, nil, "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)

	require.Equal(t, run.ID, mutexHolder(t, pg, "m1"), "the queued run should hold its mutex")

	// The run is claimed and running; a stale reaper decision now arrives.
	require.NoError(t, pg.UpsertAgent(ctx, "agent1", "h", "linux", "dev", nil, nil, nil))
	claimed, err := pg.ClaimNextRun(ctx, "agent1", nil)
	require.NoError(t, err)
	require.NotNil(t, claimed)

	updated, err := pg.FinishRunIfStatus(ctx, run.ID, api.RunQueued, api.RunFailed)
	require.NoError(t, err)
	require.False(t, updated)

	assert.Equal(t, run.ID, mutexHolder(t, pg, "m1"),
		"a caller that did not terminalize the run must not release the run's mutex")
}

// The same gating on the plain FinishRun path: a finish report that lost to an
// earlier terminal write owns nothing and must release nothing.
func TestFinishRun_LateFinishDoesNotReleaseAnotherWritersLocks(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(mutexJobSpec))
	require.NoError(t, err)
	first, err := pg.CreateRun(ctx, "j", nil, []byte(mutexJobSpec), nil, nil, "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)

	// First run finishes and releases the mutex; a successor takes it.
	updated, err := pg.FinishRun(ctx, first.ID, api.RunSucceeded)
	require.NoError(t, err)
	require.True(t, updated)

	second, err := pg.CreateRun(ctx, "j", nil, []byte(mutexJobSpec), nil, nil, "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, second.ID, mutexHolder(t, pg, "m1"))

	// A duplicate/late finish for the FIRST run arrives. It changes no status,
	// and it must not touch the successor's lock either.
	updated, err = pg.FinishRun(ctx, first.ID, api.RunFailed)
	require.NoError(t, err)
	assert.False(t, updated)

	assert.Equal(t, second.ID, mutexHolder(t, pg, "m1"), "the successor still holds the mutex")
}
