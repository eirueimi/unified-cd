package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResetStuckSyncingAppSources verifies that an AppSource stuck in "Syncing"
// longer than the threshold is reset to a retryable state, while a freshly-set
// Syncing row is left untouched.
func TestResetStuckSyncingAppSources(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	spec := []byte(`{"repoURL":"https://github.com/org/repo","targetRevision":"main","path":"jobs/"}`)

	// stuck: Syncing, updated_at is old.
	_, err := pg.UpsertAppSource(ctx, "stuck", spec)
	require.NoError(t, err)
	require.NoError(t, pg.SetAppSourceSyncStatus(ctx, "stuck", "Syncing", ""))
	// Give it a real last_commit so we can prove the reset clears it (triggering re-sync).
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "stuck", "abc123", time.Now(), nil))
	require.NoError(t, pg.SetAppSourceSyncStatus(ctx, "stuck", "Syncing", ""))
	_, err = pg.pool.Exec(ctx,
		`UPDATE app_sources SET updated_at = NOW() - interval '10 minutes' WHERE name = $1`, "stuck")
	require.NoError(t, err)

	// fresh: Syncing, updated_at is now (within the threshold).
	_, err = pg.UpsertAppSource(ctx, "fresh", spec)
	require.NoError(t, err)
	require.NoError(t, pg.SetAppSourceSyncStatus(ctx, "fresh", "Syncing", ""))

	// synced: not Syncing at all, old timestamp -- must never be touched.
	_, err = pg.UpsertAppSource(ctx, "synced", spec)
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "synced", "def456", time.Now(), nil))
	_, err = pg.pool.Exec(ctx,
		`UPDATE app_sources SET updated_at = NOW() - interval '10 minutes' WHERE name = $1`, "synced")
	require.NoError(t, err)

	n, err := pg.ResetStuckSyncingAppSources(ctx, 5*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "exactly one stuck row must be reset")

	// stuck row was reset: last_commit cleared so shouldSync retries, status no longer Syncing.
	stuck, err := pg.GetAppSource(ctx, "stuck")
	require.NoError(t, err)
	assert.Equal(t, "", stuck.LastCommit, "last_commit must be cleared so the next tick re-syncs")
	assert.NotEqual(t, "Syncing", stuck.SyncStatus, "stuck row must no longer be Syncing")
	assert.NotEmpty(t, stuck.LastError, "a reset row must record why it was reset")

	// fresh row untouched.
	fresh, err := pg.GetAppSource(ctx, "fresh")
	require.NoError(t, err)
	assert.Equal(t, "Syncing", fresh.SyncStatus, "fresh Syncing row must not be reset")

	// synced row untouched.
	synced, err := pg.GetAppSource(ctx, "synced")
	require.NoError(t, err)
	assert.Equal(t, "Synced", synced.SyncStatus)
	assert.Equal(t, "def456", synced.LastCommit)
}
