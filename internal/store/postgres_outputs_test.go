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
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")

	require.NoError(t, pg.SetStepOutput(ctx, run.ID, 0, "artifact_url", "s3://bucket/a.tar.gz"))
	require.NoError(t, pg.SetStepOutput(ctx, run.ID, 0, "version", "1.2.3"))

	outputs, err := pg.GetStepOutputs(ctx, run.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, "s3://bucket/a.tar.gz", outputs["artifact_url"])
	assert.Equal(t, "1.2.3", outputs["version"])

	// upsert — overwrites the existing value
	require.NoError(t, pg.SetStepOutput(ctx, run.ID, 0, "artifact_url", "s3://bucket/b.tar.gz"))
	outputs2, _ := pg.GetStepOutputs(ctx, run.ID, 0)
	assert.Equal(t, "s3://bucket/b.tar.gz", outputs2["artifact_url"])

	// different step indices are managed independently
	require.NoError(t, pg.SetStepOutput(ctx, run.ID, 1, "result", "ok"))
	step1Outputs, _ := pg.GetStepOutputs(ctx, run.ID, 1)
	assert.Equal(t, "ok", step1Outputs["result"])
}

func TestPostgres_GetStepOutputs_Empty(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")

	outputs, err := pg.GetStepOutputs(ctx, run.ID, 0)
	require.NoError(t, err)
	assert.Empty(t, outputs)
}

func TestPostgres_RunOutputs(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")

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
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")

	outputs, err := pg.GetRunOutputs(ctx, run.ID)
	require.NoError(t, err)
	assert.Empty(t, outputs)
}
