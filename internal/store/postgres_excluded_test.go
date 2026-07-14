package store

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExcludedParam: both sweep candidate queries must honor the excluded
// list, and an empty list must exclude nothing.
func TestExcludedParam(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	mkTerminal := func() string {
		run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
		require.NoError(t, err)
		require.NoError(t, pg.MarkRunFinished(ctx, run.ID, api.RunSucceeded))
		_, err = pg.pool.Exec(ctx, `UPDATE runs SET updated_at = NOW() - interval '40 days' WHERE id = $1`, run.ID)
		require.NoError(t, err)
		return run.ID
	}
	a, b := mkTerminal(), mkTerminal()

	// ListRunsNeedingArchival
	runs, err := pg.ListRunsNeedingArchival(ctx, 10, []string{})
	require.NoError(t, err)
	ids := []string{}
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	assert.Contains(t, ids, a)
	assert.Contains(t, ids, b)

	runs, err = pg.ListRunsNeedingArchival(ctx, 10, []string{a})
	require.NoError(t, err)
	ids = ids[:0]
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	assert.NotContains(t, ids, a)
	assert.Contains(t, ids, b)

	// ListExpiredRuns
	cutoff := time.Now().AddDate(0, 0, -30)
	got, err := pg.ListExpiredRuns(ctx, cutoff, 10, []string{b})
	require.NoError(t, err)
	assert.Contains(t, got, a)
	assert.NotContains(t, got, b)
}
