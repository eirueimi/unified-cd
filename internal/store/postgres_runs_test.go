package store

import (
	"context"
	"errors"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostgres_GetRun_NotFound verifies that a missing run yields the typed
// ErrRunNotFound sentinel (matchable with errors.Is), so callers can map a
// genuine miss to 404 while treating other errors as infrastructure failures.
// Non-ErrNoRows errors (pool exhaustion, timeouts, dropped connections) are NOT
// wrapped as ErrRunNotFound and therefore flow through as generic errors — the
// controller maps those to 500. That fault path is not exercised here because it
// requires injecting a live DB fault, but the code path is: only pgx.ErrNoRows
// is converted to ErrRunNotFound in GetRun.
func TestPostgres_GetRun_NotFound(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.GetRun(ctx, "00000000-0000-0000-0000-000000000000")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRunNotFound), "missing run should yield ErrRunNotFound, got %v", err)
}

// TestPostgres_RunParentLinkage verifies ListChildRunIDs returns exactly the
// direct children recorded via call: step reports (child_run_id).
func TestPostgres_RunParentLinkage(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	parent, err := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	childA, err := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	childB, err := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	unrelated, err := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)

	// The parent's call: step reports record each spawned child via child_run_id.
	require.NoError(t, pg.UpsertStepReport(ctx, parent.ID, 0, 0, "call-a", "", "Succeeded", nil, nil, nil, childA.ID, "hello"))
	require.NoError(t, pg.UpsertStepReport(ctx, parent.ID, 1, 0, "call-b", "", "Succeeded", nil, nil, nil, childB.ID, "hello"))

	ids, err := pg.ListChildRunIDs(ctx, parent.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{childA.ID, childB.ID}, ids)

	none, err := pg.ListChildRunIDs(ctx, unrelated.ID)
	require.NoError(t, err)
	assert.Empty(t, none)
}

// TestPostgres_FinishRun_ReportsUpdated verifies the RowsAffected signal: a fresh
// terminal transition reports updated=true, while a subsequent finish on the now
// already-terminal run reports updated=false (the CAS matched no rows) — the
// signal handleAgentFinishRun uses to distinguish a real finish from a late/no-op.
func TestPostgres_FinishRun_ReportsUpdated(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)

	updated, err := pg.FinishRun(ctx, run.ID, api.RunSucceeded)
	require.NoError(t, err)
	assert.True(t, updated, "first finish should transition the run")

	// A second finish on the already-terminal run is a no-op.
	updated, err = pg.FinishRun(ctx, run.ID, api.RunFailed)
	require.NoError(t, err)
	assert.False(t, updated, "finishing an already-terminal run should report no-op")

	got, err := pg.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunSucceeded, got.Status, "terminal status must not be overwritten")
}

func TestPostgres_CreateAndGetRun(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	run, err := pg.CreateRun(ctx, "hello", map[string]string{"k": "v"}, []byte(`{"steps":[{"name":"s","run":"echo x"}]}`), nil, "")
	require.NoError(t, err)
	assert.Equal(t, api.RunPending, run.Status)
	assert.Equal(t, "v", run.Params["k"])

	got, err := pg.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, run.ID, got.ID)
	assert.Equal(t, api.RunPending, got.Status)
}

func TestPostgres_TransitionPendingToQueued(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	_, _ = pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, "")
	_, _ = pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, "")

	n, err := pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	runs, _ := pg.ListRunsByJob(ctx, "hello", 10)
	for _, r := range runs {
		assert.Equal(t, api.RunQueued, r.Status)
	}
}

func TestPostgres_TransitionPendingToQueued_SkipsUnresolvedUses(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "hello", nil,
		[]byte(`{"steps":[{"name":"s","uses":{"job":"git://github.com/org/repo/x.yaml@v1"}}]}`), nil, "")

	n, err := pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "run with an unresolved uses git:// URI must not be queued before RunGitResolver expands it")

	got, err := pg.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunPending, got.Status)
}

func TestPostgres_ClaimNextRun(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	_, _ = pg.CreateRun(ctx, "hello", nil, []byte(`{"steps":[{"name":"s","run":"echo x"}]}`), nil, "")
	_, _ = pg.TransitionPendingToQueued(ctx, 10)

	claimed, err := pg.ClaimNextRun(ctx, "agent-1", nil)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.NotEmpty(t, claimed.ID)

	claimed2, err := pg.ClaimNextRun(ctx, "agent-1", nil)
	require.NoError(t, err)
	assert.Nil(t, claimed2)
}

func TestPostgres_MarkRunFinished(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, "")
	_, _ = pg.TransitionPendingToQueued(ctx, 10)
	_, _ = pg.ClaimNextRun(ctx, "agent-1", nil)
	require.NoError(t, pg.MarkRunRunning(ctx, run.ID))
	require.NoError(t, pg.MarkRunFinished(ctx, run.ID, api.RunSucceeded))

	got, _ := pg.GetRun(ctx, run.ID)
	assert.Equal(t, api.RunSucceeded, got.Status)
}

func TestMarkRunFinished_Idempotent(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "hello", nil, []byte(`{"steps":[{"name":"s","run":"echo x"}]}`), nil, "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	claimed, err := pg.ClaimNextRun(ctx, "agent-1", nil)
	require.NoError(t, err)
	require.NotNil(t, claimed)

	// first transition to Cancelled
	err = pg.MarkRunFinished(ctx, run.ID, api.RunCancelled)
	require.NoError(t, err)

	// attempting to overwrite with Failed is a no-op (remains Cancelled)
	err = pg.MarkRunFinished(ctx, run.ID, api.RunFailed)
	require.NoError(t, err)

	got, err := pg.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunCancelled, got.Status, "Cancelled should not be overwritten by Failed")

	// same applies for Succeeded
	err = pg.MarkRunFinished(ctx, run.ID, api.RunSucceeded)
	require.NoError(t, err)

	got, err = pg.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunCancelled, got.Status, "Cancelled should not be overwritten by Succeeded")
}

func TestPostgres_GetRunSpec(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	specJSON := []byte(`{"steps":[{"name":"deploy","run":"echo deploy"}]}`)

	_, err := pg.UpsertJob(ctx, "deploy", "unified-cd/v1", specJSON)
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "deploy", nil, specJSON, nil, "api")
	require.NoError(t, err)

	got, err := pg.GetRunSpec(ctx, run.ID)
	require.NoError(t, err)
	assert.JSONEq(t, string(specJSON), string(got))
}

// TestPostgres_DeleteJob_CascadesToRuns verifies that deleting a Job that has a Run history
// also cascade-deletes the associated Runs via the CASCADE configuration in migration 014.
func TestPostgres_DeleteJob_CascadesToRuns(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "cascade-job", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "cascade-job", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)

	require.NoError(t, pg.DeleteJob(ctx, "cascade-job"))

	_, err = pg.GetRun(ctx, run.ID)
	assert.Error(t, err, "run should have been cascade-deleted along with its job")
}

// TestPostgres_DeleteRun verifies that deleting a Run also cascade-deletes
// the associated step_reports/logs via the existing ON DELETE CASCADE constraints.
func TestPostgres_DeleteRun(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "del-run-job", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "del-run-job", nil,
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`), nil, "")
	require.NoError(t, err)
	require.NoError(t, pg.UpsertStepReport(ctx, run.ID, 0, 0, "s", "", "Succeeded", nil, nil, nil, "", ""))

	require.NoError(t, pg.DeleteRun(ctx, run.ID))

	_, err = pg.GetRun(ctx, run.ID)
	assert.Error(t, err, "run should be deleted")
	steps, err := pg.GetRunSteps(ctx, run.ID)
	require.NoError(t, err)
	assert.Empty(t, steps, "step_reports should be cascade-deleted with the run")
}

func TestPostgres_ListActiveRuns(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "job-a", "unified-cd/v1", []byte(`{}`))
	_, _ = pg.UpsertJob(ctx, "job-b", "unified-cd/v1", []byte(`{}`))

	// Active Runs (created in Pending state)
	r1, _ := pg.CreateRun(ctx, "job-a", nil, []byte(`{}`), nil, "")
	r2, _ := pg.CreateRun(ctx, "job-b", nil, []byte(`{}`), nil, "")
	// A Run in a terminal state
	r3, _ := pg.CreateRun(ctx, "job-a", nil, []byte(`{}`), nil, "")
	_ = pg.MarkRunFinished(ctx, r3.ID, api.RunSucceeded)

	runs, err := pg.ListActiveRuns(ctx)
	require.NoError(t, err)

	ids := make([]string, len(runs))
	for i, r := range runs { ids[i] = r.ID }
	assert.Contains(t, ids, r1.ID)
	assert.Contains(t, ids, r2.ID)
	assert.NotContains(t, ids, r3.ID)
}
