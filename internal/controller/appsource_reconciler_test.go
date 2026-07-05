package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/store"
)

type mockAppSourceFetcher struct {
	sha           string
	shaErr        error
	files         map[string][]byte
	filesErr      error
	fetchDirCalls int
}

func (m *mockAppSourceFetcher) ResolveCommitSHA(_ context.Context, _, _, _, _ string) (string, error) {
	return m.sha, m.shaErr
}
func (m *mockAppSourceFetcher) FetchDir(_ context.Context, _, _, _, _, _ string) (map[string][]byte, error) {
	m.fetchDirCalls++
	return m.files, m.filesErr
}

const appSourceSpecJSON = `{"repoURL":"https://github.com/org/repo","targetRevision":"main","path":"jobs/"}`

const jobYAML = `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
spec:
  steps:
    - name: compile
      run: go build ./...
`

func TestReconciler_AppliesJobsFromGit(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(appSourceSpecJSON))
	require.NoError(t, err)

	fetcher := &mockAppSourceFetcher{
		sha:   "abc123",
		files: map[string][]byte{"jobs/build.yaml": []byte(jobYAML)},
	}

	reconcileAppSources(ctx, pg, fetcher, nil)

	job, err := pg.GetJob(ctx, "build")
	require.NoError(t, err)
	assert.Equal(t, "build", job.Name)

	src, err := pg.GetAppSource(ctx, "my-src")
	require.NoError(t, err)
	assert.Equal(t, "abc123", src.LastCommit)
	assert.NotNil(t, src.LastSyncedAt)
}

func TestReconciler_SkipsWhenSHAUnchanged(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(appSourceSpecJSON))
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-src", "abc123", time.Now(), nil))

	fetcher := &mockAppSourceFetcher{
		sha:   "abc123",
		files: map[string][]byte{"jobs/build.yaml": []byte(jobYAML)},
	}
	reconcileAppSources(ctx, pg, fetcher, nil)

	assert.Equal(t, 0, fetcher.fetchDirCalls, "FetchDir should not be called when SHA unchanged")
}

func TestReconciler_PruneDeletesRemovedJobs(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	pruneSpec := `{"repoURL":"https://github.com/org/repo","targetRevision":"main","path":"jobs/","syncPolicy":{"prune":true}}`
	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(pruneSpec))
	require.NoError(t, err)

	_, _ = pg.UpsertJob(ctx, "old-job", "unified-cd/v1", []byte(`{"steps":[{"name":"s","run":"x"}]}`))
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-src", "old-sha", time.Now().Add(-10*time.Minute), []store.ResourceRef{{Kind: "Job", Name: "old-job"}}))

	fetcher := &mockAppSourceFetcher{
		sha:   "new-sha",
		files: map[string][]byte{"jobs/build.yaml": []byte(jobYAML)},
	}

	reconcileAppSources(ctx, pg, fetcher, nil)

	_, err = pg.GetJob(ctx, "build")
	require.NoError(t, err)

	_, err = pg.GetJob(ctx, "old-job")
	require.Error(t, err)
}

func TestReconciler_WarnOnlyWithoutPrune(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(appSourceSpecJSON))
	require.NoError(t, err)

	_, _ = pg.UpsertJob(ctx, "old-job", "unified-cd/v1", []byte(`{"steps":[{"name":"s","run":"x"}]}`))
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-src", "old-sha", time.Now().Add(-10*time.Minute), []store.ResourceRef{{Kind: "Job", Name: "old-job"}}))

	fetcher := &mockAppSourceFetcher{
		sha:   "new-sha",
		files: map[string][]byte{"jobs/build.yaml": []byte(jobYAML)},
	}

	reconcileAppSources(ctx, pg, fetcher, nil)

	_, err = pg.GetJob(ctx, "old-job")
	require.NoError(t, err, "old-job should still exist when prune is false")
}

func TestReconciler_ForceSyncOnEmptyCommit(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(appSourceSpecJSON))
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "my-src", "abc123", time.Now(), nil))
	require.NoError(t, pg.ResetAppSourceCommit(ctx, "my-src"))

	fetcher := &mockAppSourceFetcher{
		sha:   "abc123",
		files: map[string][]byte{"jobs/build.yaml": []byte(jobYAML)},
	}

	reconcileAppSources(ctx, pg, fetcher, nil)

	_, err = pg.GetJob(ctx, "build")
	require.NoError(t, err, "should sync when last_commit is empty (forced sync)")
}
