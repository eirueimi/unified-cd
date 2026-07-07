package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostgres_TailLogsRecentByStep verifies the per-step tail query used by
// the WebUI's on-demand step-log backfill: only the given step's lines come
// back, capped to the most recent `limit`, in ascending seq order.
func TestPostgres_TailLogsRecentByStep(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)

	now := time.Now().UTC()
	// Interleave: 3 lines for step 0, 5 lines for step 2.
	for i := 0; i < 3; i++ {
		_, err := pg.AppendLog(ctx, run.ID, 0, "stdout", now, fmt.Sprintf("zero-%d", i))
		require.NoError(t, err)
	}
	for i := 0; i < 5; i++ {
		_, err := pg.AppendLog(ctx, run.ID, 2, "stdout", now, fmt.Sprintf("two-%d", i))
		require.NoError(t, err)
	}

	// All of step 0's lines, none of step 2's.
	lines, err := pg.TailLogsRecentByStep(ctx, run.ID, 0, 100)
	require.NoError(t, err)
	require.Len(t, lines, 3)
	for i, l := range lines {
		assert.Equal(t, 0, l.StepIndex)
		assert.Equal(t, fmt.Sprintf("zero-%d", i), l.Line, "ascending seq order")
	}

	// Cap keeps the most RECENT lines of the step.
	lines, err = pg.TailLogsRecentByStep(ctx, run.ID, 2, 2)
	require.NoError(t, err)
	require.Len(t, lines, 2)
	assert.Equal(t, "two-3", lines[0].Line)
	assert.Equal(t, "two-4", lines[1].Line)

	// Unknown step: empty, no error.
	lines, err = pg.TailLogsRecentByStep(ctx, run.ID, 9, 10)
	require.NoError(t, err)
	assert.Empty(t, lines)
}
