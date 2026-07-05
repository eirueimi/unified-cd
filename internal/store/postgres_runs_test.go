package store

import (
	"context"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	// アクティブなRun（Pending状態で作成される）
	r1, _ := pg.CreateRun(ctx, "job-a", nil, []byte(`{}`), nil, "")
	r2, _ := pg.CreateRun(ctx, "job-b", nil, []byte(`{}`), nil, "")
	// 終了状態のRun
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
