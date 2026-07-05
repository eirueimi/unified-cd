package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_AppSourceCRUD(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	spec := []byte(`{"repoURL":"https://github.com/org/repo","targetRevision":"main","path":"jobs/"}`)

	// Upsert (create)
	a, err := pg.UpsertAppSource(ctx, "my-pipelines", spec)
	require.NoError(t, err)
	assert.Equal(t, "my-pipelines", a.Name)
	assert.JSONEq(t, string(spec), string(a.Spec))
	assert.Nil(t, a.LastSyncedAt)
	assert.Equal(t, "", a.LastCommit)
	assert.Empty(t, a.ManagedJobs)

	// Get
	got, err := pg.GetAppSource(ctx, "my-pipelines")
	require.NoError(t, err)
	assert.Equal(t, "my-pipelines", got.Name)

	// List
	list, err := pg.ListAppSources(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	// UpdateAppSourceSyncState
	now := time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-pipelines", "abc123", now, []string{"job-a", "job-b"}))
	got, err = pg.GetAppSource(ctx, "my-pipelines")
	require.NoError(t, err)
	assert.Equal(t, "abc123", got.LastCommit)
	assert.WithinDuration(t, now, *got.LastSyncedAt, time.Second)
	assert.Equal(t, []string{"job-a", "job-b"}, got.ManagedJobs)

	// ResetAppSourceCommit
	require.NoError(t, pg.ResetAppSourceCommit(ctx, "my-pipelines"))
	got, err = pg.GetAppSource(ctx, "my-pipelines")
	require.NoError(t, err)
	assert.Equal(t, "", got.LastCommit)

	// Upsert (update) resets last_commit
	spec2 := []byte(`{"repoURL":"https://github.com/org/repo2","targetRevision":"main","path":"ci/"}`)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-pipelines", "def456", now, nil))
	_, err = pg.UpsertAppSource(ctx, "my-pipelines", spec2)
	require.NoError(t, err)
	got, err = pg.GetAppSource(ctx, "my-pipelines")
	require.NoError(t, err)
	assert.Equal(t, "", got.LastCommit, "upsert should reset last_commit")

	// Delete
	require.NoError(t, pg.DeleteAppSource(ctx, "my-pipelines"))
	_, err = pg.GetAppSource(ctx, "my-pipelines")
	require.Error(t, err)
}

func TestAppSource_SyncStatusLifecycle(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertAppSource(ctx, "s1", []byte(`{}`))
	require.NoError(t, err)

	// initial values are empty
	got, err := pg.GetAppSource(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, "", got.SyncStatus)
	assert.Equal(t, "", got.LastError)

	// transition to Syncing
	require.NoError(t, pg.SetAppSourceSyncStatus(ctx, "s1", "Syncing", ""))
	got, err = pg.GetAppSource(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, "Syncing", got.SyncStatus)

	// Failed + error
	require.NoError(t, pg.SetAppSourceSyncStatus(ctx, "s1", "Failed", "boom"))
	got, err = pg.GetAppSource(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, "Failed", got.SyncStatus)
	assert.Equal(t, "boom", got.LastError)

	// recording a successful sync sets Synced and clears the error
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "s1", "sha1", time.Now(), []string{"j"}))
	got, err = pg.GetAppSource(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, "Synced", got.SyncStatus)
	assert.Equal(t, "", got.LastError)
	assert.Equal(t, "sha1", got.LastCommit)
}

func TestPostgres_DeleteJob(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	require.NoError(t, pg.DeleteJob(ctx, "hello"))

	_, err = pg.GetJob(ctx, "hello")
	require.Error(t, err)
}
