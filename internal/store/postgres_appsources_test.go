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

func TestPostgres_DeleteJob(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	require.NoError(t, pg.DeleteJob(ctx, "hello"))

	_, err = pg.GetJob(ctx, "hello")
	require.Error(t, err)
}
