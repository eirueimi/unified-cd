package controller

import (
	"context"
	"fmt"
	"strings"
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
	bo := newFailureBackoff(time.Minute, time.Hour, 10_000)
	resolveGitPendingRuns(t.Context(), pg, resolver, nil, bo, time.Hour)

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
	bo := newFailureBackoff(time.Minute, time.Hour, 10_000)
	resolveGitPendingRuns(t.Context(), pg, resolver, nil, bo, time.Hour)

	got, err := pg.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunPending, got.Status)
}

// TestResolveGitPendingRuns_TransientErrorRecordsBackoffFailure verifies a
// transient resolve failure is recorded against the backoff tracker, so a
// subsequent call with the same backoff instance excludes the run from the
// candidate batch (it is not re-resolved every tick while poisoned).
func TestResolveGitPendingRuns_TransientErrorRecordsBackoffFailure(t *testing.T) {
	pg := store.NewTestPostgres(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	specJSON := []byte(`{"steps":[{"name":"tpl","uses":{"job":"git://github.com/org/repo/job.yaml@v1"}}]}`)
	run, err := pg.CreateRun(t.Context(), "j", nil, specJSON, nil, nil, "")
	require.NoError(t, err)

	resolver := gittemplate.NewResolver(erroringFetcher{}, nil)
	bo := newFailureBackoff(time.Minute, time.Hour, 10_000)
	resolveGitPendingRuns(t.Context(), pg, resolver, nil, bo, time.Hour)

	excluded := bo.Excluded(time.Now())
	assert.Contains(t, excluded, run.ID, "a transient failure must be recorded against the backoff tracker")
}

// TestResolveGitPendingRuns_DeadlineExceededFailsRun verifies a run whose
// git-template resolution has kept failing longer than the deadline is
// Failed (instead of staying Pending forever) with a system log line
// explaining why.
func TestResolveGitPendingRuns_DeadlineExceededFailsRun(t *testing.T) {
	pg := store.NewTestPostgres(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	specJSON := []byte(`{"steps":[{"name":"tpl","uses":{"job":"git://github.com/org/repo/job.yaml@v1"}}]}`)
	run, err := pg.CreateRun(t.Context(), "j", nil, specJSON, nil, nil, "")
	require.NoError(t, err)

	resolver := gittemplate.NewResolver(erroringFetcher{}, nil)

	// First call: within the deadline, the run stays Pending and the
	// failure is recorded against the backoff tracker.
	bo := newFailureBackoff(time.Minute, time.Hour, 10_000)
	resolveGitPendingRuns(t.Context(), pg, resolver, nil, bo, time.Hour)

	got, err := pg.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunPending, got.Status)

	// Backdate created_at past the deadline and retry with a FRESH backoff
	// (a real restart would also start fresh) so the run isn't excluded by
	// the first call's recorded failure.
	require.NoError(t, pg.BackdateRunCreatedAt(t.Context(), run.ID, 2*time.Hour))
	bo2 := newFailureBackoff(time.Minute, time.Hour, 10_000)
	resolveGitPendingRuns(t.Context(), pg, resolver, nil, bo2, time.Hour)

	got, err = pg.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunFailed, got.Status)

	logs, err := pg.TailLogs(t.Context(), run.ID, 0, 100)
	require.NoError(t, err)
	found := false
	for _, l := range logs {
		if strings.Contains(l.Line, "git template resolution failed for more than") {
			found = true
			break
		}
	}
	assert.True(t, found, "expected a system log line about the exceeded resolve deadline, got: %+v", logs)
}

type badYAMLFetcher struct{}

func (badYAMLFetcher) Fetch(ctx context.Context, uri gittemplate.URI, token, sshKey string) ([]byte, error) {
	return []byte("not a job manifest"), nil
}

type erroringFetcher struct{}

func (erroringFetcher) Fetch(ctx context.Context, uri gittemplate.URI, token, sshKey string) ([]byte, error) {
	return nil, fmt.Errorf("network unreachable")
}
