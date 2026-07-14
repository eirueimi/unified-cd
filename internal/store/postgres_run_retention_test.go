package store

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostgres_ListExpiredRuns verifies selection (terminal + older than
// cutoff), exclusion (recent rows, non-terminal rows), oldest-first order,
// and LIMIT.
func TestPostgres_ListExpiredRuns(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	// mkRun creates a run, optionally finishes it, and backdates updated_at.
	// A terminal run's updated_at never changes again, so it is the finish time.
	mkRun := func(status api.RunStatus, age string) string {
		t.Helper()
		run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
		require.NoError(t, err)
		if status != "" {
			require.NoError(t, pg.MarkRunFinished(ctx, run.ID, status))
		}
		if age != "" {
			_, err = pg.pool.Exec(ctx,
				`UPDATE runs SET updated_at = NOW() - $1::interval WHERE id = $2`, age, run.ID)
			require.NoError(t, err)
		}
		return run.ID
	}

	oldest := mkRun(api.RunSucceeded, "40 days")
	older := mkRun(api.RunFailed, "35 days")
	oldCancelled := mkRun(api.RunCancelled, "31 days")
	_ = mkRun(api.RunSucceeded, "10 days") // recent: kept
	_ = mkRun("", "40 days")               // old but still Pending: kept

	cutoff := time.Now().AddDate(0, 0, -30)

	ids, err := pg.ListExpiredRuns(ctx, cutoff, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{oldest, older, oldCancelled}, ids, "terminal+old only, oldest first")

	// LIMIT is respected and keeps the oldest-first prefix.
	ids, err = pg.ListExpiredRuns(ctx, cutoff, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{oldest, older}, ids)
}
