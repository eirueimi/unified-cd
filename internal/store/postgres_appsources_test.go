package store

import (
	"context"
	"reflect"
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
	assert.Empty(t, a.ManagedResources)

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
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-pipelines", "abc123", now, []ResourceRef{{Kind: "Job", Name: "job-a"}, {Kind: "Job", Name: "job-b"}}))
	got, err = pg.GetAppSource(ctx, "my-pipelines")
	require.NoError(t, err)
	assert.Equal(t, "abc123", got.LastCommit)
	assert.WithinDuration(t, now, *got.LastSyncedAt, time.Second)
	assert.Equal(t, []ResourceRef{{Kind: "Job", Name: "job-a"}, {Kind: "Job", Name: "job-b"}}, got.ManagedResources)

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

func TestAppSource_ManagedResourcesRoundTrip(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	if _, err := pg.UpsertAppSource(ctx, "src1", []byte(`{"repoURL":"https://x/y","targetRevision":"main","path":"jobs"}`)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	want := []ResourceRef{{Kind: "Job", Name: "build"}, {Kind: "Schedule", Name: "nightly"}}
	if err := pg.UpdateAppSourceSyncState(ctx, "src1", "sha1", time.Now(), want); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := pg.GetAppSource(ctx, "src1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(got.ManagedResources, want) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got.ManagedResources, want)
	}
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
