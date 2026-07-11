package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstrumentedStoreCountsTransitions(t *testing.T) {
	m := New()
	pg := store.NewTestPostgres(t)
	st := NewInstrumentedStore(pg, m)
	ctx := context.Background()

	// Create a job first (required for foreign key)
	_, err := pg.UpsertJob(ctx, "job-a", "v1", []byte(`{}`))
	require.NoError(t, err)

	run, err := st.CreateRun(ctx, "job-a", nil, []byte(`{}`), nil, nil, "webhook:push")
	require.NoError(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(m.runsCreated.WithLabelValues("webhook")))

	// First finish counts; a second finish attempt is CAS-rejected and must not.
	require.NoError(t, st.MarkRunFinished(ctx, run.ID, api.RunFailed))
	require.NoError(t, st.MarkRunFinished(ctx, run.ID, api.RunSucceeded))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.runsFinished.WithLabelValues("Failed")))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.runsFinished.WithLabelValues("Succeeded")))

	// Step reports: Running is not counted; terminal is; duration observed
	// only when both timestamps are present.
	started := time.Now().Add(-30 * time.Second)
	ended := time.Now()
	require.NoError(t, st.UpsertStepReport(ctx, run.ID, 0, 0, "build", "", "Running", nil, &started, nil, "", ""))
	require.NoError(t, st.UpsertStepReport(ctx, run.ID, 0, 0, "build", "", "Succeeded", nil, &started, &ended, "", ""))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.stepsCompleted.WithLabelValues("Running")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.stepsCompleted.WithLabelValues("Succeeded")))
}

func TestInstrumentedStoreDoesNotCountFailures(t *testing.T) {
	m := New()
	pg := store.NewTestPostgres(t)
	st := NewInstrumentedStore(pg, m)

	// Finishing a nonexistent run errors and must not count.
	err := st.MarkRunFinished(context.Background(), "00000000-0000-0000-0000-000000000000", api.RunFailed)
	_ = err // MarkRunFinished's error contract is the store's own; the metric is what we assert.
	assert.Equal(t, 0.0, testutil.ToFloat64(m.runsFinished.WithLabelValues("Failed")))
}
