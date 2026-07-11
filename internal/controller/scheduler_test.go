package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScheduler_MovesPendingToQueued(t *testing.T) {
	pg := store.NewTestPostgres(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	_, _ = pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunScheduler(ctx, pg, 50*time.Millisecond)

	require.Eventually(t, func() bool {
		runs, _ := pg.ListRunsByJob(ctx, "j", 10)
		return len(runs) == 1 && runs[0].Status == api.RunQueued
	}, 3*time.Second, 100*time.Millisecond)
}

func TestScheduler_OnlyOneLeaderQueues(t *testing.T) {
	pg := store.NewTestPostgres(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	_, _ = pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RunScheduler(ctx, pg, 30*time.Millisecond)
	go RunScheduler(ctx, pg, 30*time.Millisecond)

	require.Eventually(t, func() bool {
		runs, _ := pg.ListRunsByJob(ctx, "j", 10)
		return len(runs) == 1 && runs[0].Status == api.RunQueued
	}, 3*time.Second, 100*time.Millisecond)

	runs, _ := pg.ListRunsByJob(ctx, "j", 10)
	require.Equal(t, 1, len(runs))
}

func TestRunCacheCleanup_SkipsOnNilObj(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// Returns immediately when obj is nil (does not panic).
	RunCacheCleanup(ctx, nil, nil)
}

func TestRunCacheCleanup_SkipsOnNilSt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// Returns immediately when st is nil (does not panic).
	RunCacheCleanup(ctx, nil, nil)
}

func TestResolveGitPendingRuns_DeterministicErrorFailsRun(t *testing.T) {
	pg := store.NewTestPostgres(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	specJSON := []byte(`{"steps":[{"name":"tpl","uses":{"job":"git://github.com/org/repo/job.yaml@v1"}}]}`)
	run, err := pg.CreateRun(t.Context(), "j", nil, specJSON, nil, nil, "")
	require.NoError(t, err)

	resolver := gittemplate.NewResolver(badYAMLFetcher{}, nil)
	resolveGitPendingRuns(t.Context(), pg, resolver, nil)

	got, err := pg.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunFailed, got.Status)
}

func TestResolveGitPendingRuns_TransientErrorKeepsPending(t *testing.T) {
	pg := store.NewTestPostgres(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	specJSON := []byte(`{"steps":[{"name":"tpl","uses":{"job":"git://github.com/org/repo/job.yaml@v1"}}]}`)
	run, err := pg.CreateRun(t.Context(), "j", nil, specJSON, nil, nil, "")
	require.NoError(t, err)

	resolver := gittemplate.NewResolver(erroringFetcher{}, nil)
	resolveGitPendingRuns(t.Context(), pg, resolver, nil)

	got, err := pg.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunPending, got.Status)
}

type badYAMLFetcher struct{}

func (badYAMLFetcher) Fetch(ctx context.Context, uri gittemplate.URI, token, sshKey string) ([]byte, error) {
	return []byte("not a job manifest"), nil
}

type erroringFetcher struct{}

func (erroringFetcher) Fetch(ctx context.Context, uri gittemplate.URI, token, sshKey string) ([]byte, error) {
	return nil, fmt.Errorf("network unreachable")
}
