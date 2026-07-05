package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_TailLogsRecent(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	now := time.Now().UTC()
	for _, ln := range []string{"a", "b", "c", "d", "e"} {
		_, err := pg.AppendLog(ctx, run.ID, 0, "stdout", now, ln)
		require.NoError(t, err)
	}

	// A limit smaller than the log returns the TAIL, in ascending order.
	recent, err := pg.TailLogsRecent(ctx, run.ID, 2)
	require.NoError(t, err)
	require.Len(t, recent, 2)
	assert.Equal(t, "d", recent[0].Line)
	assert.Equal(t, "e", recent[1].Line)

	// A limit larger than the log returns everything, ascending.
	all, err := pg.TailLogsRecent(ctx, run.ID, 100)
	require.NoError(t, err)
	require.Len(t, all, 5)
	assert.Equal(t, "a", all[0].Line)
	assert.Equal(t, "e", all[4].Line)
}

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

	require.NoError(t, pg.UpsertStepReport(ctx, run.ID, 0, 0, "step-one", "", "Running", nil, nil, nil, "", ""))
	ec := 0
	end := time.Now().UTC()
	require.NoError(t, pg.UpsertStepReport(ctx, run.ID, 0, 0, "step-one", "", "Succeeded", &ec, nil, &end, "", ""))
}

func TestPostgres_UpsertStepReport_StageIndex(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")

	exitCode := 0
	now := time.Now().UTC()
	err := pg.UpsertStepReport(ctx, run.ID, 0, 1, "build", "", "Succeeded", &exitCode, &now, &now, "", "")
	require.NoError(t, err)

	steps, err := pg.GetRunSteps(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, steps, 1)
	assert.Equal(t, 1, steps[0].StageIndex)
}

func TestStepReports_MatrixVariantsDoNotClobber(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")

	ec := 0
	now := time.Now().UTC()
	require.NoError(t, pg.UpsertStepReport(ctx, run.ID, 0, 0, "build (linux, amd64)", "linux/amd64", "Succeeded", &ec, &now, &now, "", ""))
	require.NoError(t, pg.UpsertStepReport(ctx, run.ID, 0, 0, "build (linux, arm64)", "linux/arm64", "Running", nil, &now, nil, "", ""))

	steps, err := pg.GetRunSteps(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, steps, 2) // same step_index but 2 rows, one per variant
	assert.Equal(t, "linux/amd64", steps[0].Variant)
	assert.Equal(t, "linux/arm64", steps[1].Variant)

	// same (run, index, variant) upserts in place
	require.NoError(t, pg.UpsertStepReport(ctx, run.ID, 0, 0, "build (linux, arm64)", "linux/arm64", "Succeeded", &ec, &now, &now, "", ""))
	steps, err = pg.GetRunSteps(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, steps, 2)
}

func TestPostgres_Agents(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	require.NoError(t, pg.UpsertAgent(ctx, "a1", "host1", "linux", "dev", []string{"build"}, nil))
	require.NoError(t, pg.TouchAgent(ctx, "a1"))
}
