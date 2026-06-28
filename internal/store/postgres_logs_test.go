package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_AppendAndTailLogs(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")

	now := time.Now().UTC()
	seq1, err := pg.AppendLog(ctx, run.ID, 0, "stdout", now, "hello")
	require.NoError(t, err)
	seq2, err := pg.AppendLog(ctx, run.ID, 0, "stdout", now, "world")
	require.NoError(t, err)
	assert.Greater(t, seq2, seq1)

	lines, err := pg.TailLogs(ctx, run.ID, 0, 100)
	require.NoError(t, err)
	require.Len(t, lines, 2)
	assert.Equal(t, "hello", lines[0].Line)
	assert.Equal(t, "world", lines[1].Line)

	tail, err := pg.TailLogs(ctx, run.ID, seq1, 100)
	require.NoError(t, err)
	require.Len(t, tail, 1)
	assert.Equal(t, "world", tail[0].Line)
}

func TestPostgres_UpsertStepReport(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")

	require.NoError(t, pg.UpsertStepReport(ctx, run.ID, 0, 0, "step-one", "Running", nil, nil, nil))
	ec := 0
	end := time.Now().UTC()
	require.NoError(t, pg.UpsertStepReport(ctx, run.ID, 0, 0, "step-one", "Succeeded", &ec, nil, &end))
}

func TestPostgres_UpsertStepReport_StageIndex(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")

	exitCode := 0
	now := time.Now().UTC()
	err := pg.UpsertStepReport(ctx, run.ID, 0, 1, "build", "Succeeded", &exitCode, &now, &now)
	require.NoError(t, err)

	steps, err := pg.GetRunSteps(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, steps, 1)
	assert.Equal(t, 1, steps[0].StageIndex)
}

func TestPostgres_Agents(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	require.NoError(t, pg.UpsertAgent(ctx, "a1", "host1", "linux", "dev", []string{"build"}, nil))
	require.NoError(t, pg.TouchAgent(ctx, "a1"))
}
