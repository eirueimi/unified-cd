package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_StepOutputs(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "", "")

	require.NoError(t, pg.SetStepOutput(ctx, run.ID, 0, "", "artifact_url", "s3://bucket/a.tar.gz"))
	require.NoError(t, pg.SetStepOutput(ctx, run.ID, 0, "", "version", "1.2.3"))

	outputs, err := pg.GetStepOutputs(ctx, run.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, "s3://bucket/a.tar.gz", outputs["artifact_url"])
	assert.Equal(t, "1.2.3", outputs["version"])

	// upsert — overwrites the existing value
	require.NoError(t, pg.SetStepOutput(ctx, run.ID, 0, "", "artifact_url", "s3://bucket/b.tar.gz"))
	outputs2, _ := pg.GetStepOutputs(ctx, run.ID, 0)
	assert.Equal(t, "s3://bucket/b.tar.gz", outputs2["artifact_url"])

	// different step indices are managed independently
	require.NoError(t, pg.SetStepOutput(ctx, run.ID, 1, "", "result", "ok"))
	step1Outputs, _ := pg.GetStepOutputs(ctx, run.ID, 1)
	assert.Equal(t, "ok", step1Outputs["result"])
}

func TestStepOutputs_VariantKeyed(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "", "")

	require.NoError(t, pg.SetStepOutput(ctx, run.ID, 0, "linux/amd64", "version", "1.2"))
	require.NoError(t, pg.SetStepOutput(ctx, run.ID, 0, "linux/arm64", "version", "1.3"))
	// A different step index must not leak into the inspection below.
	require.NoError(t, pg.SetStepOutput(ctx, run.ID, 1, "linux/amd64", "version", "other"))

	// GetStepOutputs is variant-unaware, so inspect the rows directly: both
	// variant rows must exist with their own values (no clobbering).
	rows, err := pg.pool.Query(ctx,
		`SELECT variant, key, value FROM step_outputs WHERE run_id = $1 AND step_index = 0 ORDER BY variant, key`,
		run.ID)
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]string{}
	n := 0
	for rows.Next() {
		var variant, key, value string
		require.NoError(t, rows.Scan(&variant, &key, &value))
		assert.Equal(t, "version", key)
		got[variant] = value
		n++
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, 2, n, "exactly one row per variant")
	assert.Equal(t, map[string]string{"linux/amd64": "1.2", "linux/arm64": "1.3"}, got)

	// Re-setting a variant overwrites only that variant's row.
	require.NoError(t, pg.SetStepOutput(ctx, run.ID, 0, "linux/amd64", "version", "1.4"))
	var cnt int
	require.NoError(t, pg.pool.QueryRow(ctx,
		`SELECT count(*) FROM step_outputs WHERE run_id = $1 AND step_index = 0`, run.ID).Scan(&cnt))
	assert.Equal(t, 2, cnt)
	var v string
	require.NoError(t, pg.pool.QueryRow(ctx,
		`SELECT value FROM step_outputs WHERE run_id = $1 AND step_index = 0 AND variant = 'linux/arm64'`, run.ID).Scan(&v))
	assert.Equal(t, "1.3", v)
}

func TestPostgres_GetStepOutputs_Empty(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "", "")

	outputs, err := pg.GetStepOutputs(ctx, run.ID, 0)
	require.NoError(t, err)
	assert.Empty(t, outputs)
}

func TestPostgres_RunOutputs(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "", "")

	require.NoError(t, pg.SetRunOutput(ctx, run.ID, "artifact_url", "s3://bucket/a.tar.gz"))
	require.NoError(t, pg.SetRunOutput(ctx, run.ID, "version", "1.2.3"))

	outputs, err := pg.GetRunOutputs(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, "s3://bucket/a.tar.gz", outputs["artifact_url"])
	assert.Equal(t, "1.2.3", outputs["version"])
}

func TestPostgres_GetRunOutputs_Empty(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "", "")

	outputs, err := pg.GetRunOutputs(ctx, run.ID)
	require.NoError(t, err)
	assert.Empty(t, outputs)
}
