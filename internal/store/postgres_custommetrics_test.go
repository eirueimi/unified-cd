package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
)

func newMetricsRun(t *testing.T, pg *Postgres) *api.Run {
	t.Helper()
	_, _ = pg.UpsertJob(context.Background(), "j", "unified-cd/v1", []byte(`{}`), JobMeta{})
	run, err := pg.CreateRun(context.Background(), "j", nil, []byte(`{}`), nil, nil, "", "", RunOrigin{})
	require.NoError(t, err)
	return run
}

func TestApplyMetricGauge_SetAddSub(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	g := MetricGauge{Name: "m", Help: "h", Labels: map[string]string{"a": "b"}}

	g.Value = 5
	require.NoError(t, pg.ApplyMetricGauge(ctx, g, "h1", "Set", 0))
	g.Value = 2
	require.NoError(t, pg.ApplyMetricGauge(ctx, g, "h1", "Add", 0))
	g.Value = 1
	require.NoError(t, pg.ApplyMetricGauge(ctx, g, "h1", "Sub", 0))

	got, err := pg.ListMetricGauges(ctx, time.Time{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 6.0, got[0].Value)
	assert.Equal(t, map[string]string{"a": "b"}, got[0].Labels)
	assert.Equal(t, "h", got[0].Help)

	// Add / Sub on a brand new series start from zero.
	g.Value = 3
	require.NoError(t, pg.ApplyMetricGauge(ctx, g, "h2", "Sub", 0))
	got, _ = pg.ListMetricGauges(ctx, time.Time{})
	require.Len(t, got, 2)
	assert.Equal(t, -3.0, got[1].Value)

	assert.Error(t, pg.ApplyMetricGauge(ctx, g, "h1", "Mul", 0))
}

func TestApplyMetricGauge_SeriesCap(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	g := MetricGauge{Name: "m", Value: 1}

	require.NoError(t, pg.ApplyMetricGauge(ctx, g, "h1", "Set", 2))
	require.NoError(t, pg.ApplyMetricGauge(ctx, g, "h2", "Set", 2))
	assert.ErrorIs(t, pg.ApplyMetricGauge(ctx, g, "h3", "Set", 2), ErrSeriesCap)
	// An existing series still updates at the cap.
	g.Value = 9
	require.NoError(t, pg.ApplyMetricGauge(ctx, g, "h1", "Set", 2))
	// The cap is per name.
	other := MetricGauge{Name: "n", Value: 1}
	require.NoError(t, pg.ApplyMetricGauge(ctx, other, "h3", "Set", 2))

	got, _ := pg.ListMetricGauges(ctx, time.Time{})
	assert.Len(t, got, 3)
}

func TestListMetricGauges_Since(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.ApplyMetricGauge(ctx, MetricGauge{Name: "old", Value: 1}, "h", "Set", 0))
	require.NoError(t, pg.ApplyMetricGauge(ctx, MetricGauge{Name: "new", Value: 1}, "h", "Set", 0))
	_, err := pg.pool.Exec(ctx, `UPDATE custom_metric_gauges SET updated_at = now() - interval '2 hours' WHERE name = 'old'`)
	require.NoError(t, err)

	got, err := pg.ListMetricGauges(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "new", got[0].Name)

	n, err := pg.DeleteMetricGaugesBefore(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	got, _ = pg.ListMetricGauges(ctx, time.Time{})
	assert.Len(t, got, 1)
}

func TestMetricRealtime_PutListDelete(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	run := newMetricsRun(t, pg)
	at := time.Now().UTC().Truncate(time.Millisecond)

	require.NoError(t, pg.PutMetricRealtime(ctx, MetricRealtime{RunID: run.ID, StepKey: "", Name: "run_t", Help: "h", Labels: map[string]string{"k": "v"}, StartedAt: at}))
	require.NoError(t, pg.PutMetricRealtime(ctx, MetricRealtime{RunID: run.ID, StepKey: "0/1/", Name: "step_t", StartedAt: at}))
	// Re-put overwrites.
	require.NoError(t, pg.PutMetricRealtime(ctx, MetricRealtime{RunID: run.ID, StepKey: "0/1/", Name: "step_t", Labels: map[string]string{"x": "y"}, StartedAt: at}))
	// Unknown run is a no-op.
	require.NoError(t, pg.PutMetricRealtime(ctx, MetricRealtime{RunID: "00000000-0000-0000-0000-000000000000", Name: "z", StartedAt: at}))

	got, err := pg.ListMetricRealtime(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "", got[0].StepKey)
	assert.Equal(t, map[string]string{"k": "v"}, got[0].Labels)
	assert.True(t, at.Equal(got[0].StartedAt))
	assert.Equal(t, map[string]string{"x": "y"}, got[1].Labels)

	// A run-level delete leaves step rows alone.
	require.NoError(t, pg.DeleteMetricRealtime(ctx, run.ID, ""))
	got, _ = pg.ListMetricRealtime(ctx)
	require.Len(t, got, 1)
	assert.Equal(t, "0/1/", got[0].StepKey)

	require.NoError(t, pg.DeleteMetricRealtime(ctx, run.ID, "0/1/"))
	got, _ = pg.ListMetricRealtime(ctx)
	assert.Empty(t, got)
}

func TestDeleteMetricRealtimeForTerminalRuns(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	done := newMetricsRun(t, pg)
	live := newMetricsRun(t, pg)
	at := time.Now().UTC()
	for _, id := range []string{done.ID, live.ID} {
		require.NoError(t, pg.PutMetricRealtime(ctx, MetricRealtime{RunID: id, Name: "t", StartedAt: at}))
	}
	ok, err := pg.FinishRun(ctx, done.ID, api.RunSucceeded)
	require.NoError(t, err)
	require.True(t, ok)

	n, err := pg.DeleteMetricRealtimeForTerminalRuns(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	got, _ := pg.ListMetricRealtime(ctx)
	require.Len(t, got, 1)
	assert.Equal(t, live.ID, got[0].RunID)
}

func TestUpsertStepReport_FirstTerminal(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	run := newMetricsRun(t, pg)

	first, err := pg.UpsertStepReport(ctx, run.ID, 0, 0, "s", "", "Running", nil, nil, nil, "", "")
	require.NoError(t, err)
	assert.False(t, first)
	first, err = pg.UpsertStepReport(ctx, run.ID, 0, 0, "s", "", "Failed", nil, nil, nil, "", "")
	require.NoError(t, err)
	assert.True(t, first)
	first, err = pg.UpsertStepReport(ctx, run.ID, 0, 0, "s", "", "Failed", nil, nil, nil, "", "")
	require.NoError(t, err)
	assert.False(t, first)

	// No prior row and a terminal status: first terminal.
	first, err = pg.UpsertStepReport(ctx, run.ID, 1, 0, "t", "", "Skipped", nil, nil, nil, "", "")
	require.NoError(t, err)
	assert.True(t, first)
}

func TestGetStepVariantOutputs(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	run := newMetricsRun(t, pg)

	require.NoError(t, pg.SetStepOutput(ctx, run.ID, 0, "linux", "k", "L"))
	require.NoError(t, pg.SetStepOutput(ctx, run.ID, 0, "linux", "only", "1"))
	require.NoError(t, pg.SetStepOutput(ctx, run.ID, 0, "mac", "k", "M"))
	require.NoError(t, pg.SetStepOutput(ctx, run.ID, 0, "", "k", "plain"))

	got, err := pg.GetStepVariantOutputs(ctx, run.ID, 0, "linux")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"k": "L", "only": "1"}, got)
	got, _ = pg.GetStepVariantOutputs(ctx, run.ID, 0, "mac")
	assert.Equal(t, map[string]string{"k": "M"}, got)
	got, _ = pg.GetStepVariantOutputs(ctx, run.ID, 0, "")
	assert.Equal(t, map[string]string{"k": "plain"}, got)
	got, _ = pg.GetStepVariantOutputs(ctx, run.ID, 0, "none")
	assert.Empty(t, got)
}

func TestUpsertStepReport_RunningTakesHotPath(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	run := newMetricsRun(t, pg)

	first, err := pg.UpsertStepReport(ctx, run.ID, 0, 0, "s", "", "Running", nil, nil, nil, "", "")
	require.NoError(t, err)
	assert.False(t, first)

	var status string
	require.NoError(t, pg.pool.QueryRow(ctx,
		`SELECT status FROM step_reports WHERE run_id = $1 AND step_index = 0`, run.ID).Scan(&status))
	assert.Equal(t, "Running", status)

	// A missing run still surfaces ErrRunNotFound on the hot path.
	_, err = pg.UpsertStepReport(ctx, "00000000-0000-0000-0000-000000000000", 0, 0, "s", "", "Running", nil, nil, nil, "", "")
	require.ErrorIs(t, err, ErrRunNotFound)
}

func TestListMetricRealtime_HidesTerminalRuns(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	done := newMetricsRun(t, pg)
	live := newMetricsRun(t, pg)
	at := time.Now().UTC()
	for _, id := range []string{done.ID, live.ID} {
		require.NoError(t, pg.PutMetricRealtime(ctx, MetricRealtime{RunID: id, Name: "t", StartedAt: at}))
	}
	ok, err := pg.FinishRun(ctx, done.ID, api.RunSucceeded)
	require.NoError(t, err)
	require.True(t, ok)

	got, err := pg.ListMetricRealtime(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, live.ID, got[0].RunID)
}
