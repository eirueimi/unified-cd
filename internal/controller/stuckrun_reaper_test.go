package controller

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
)

// fakeReaperStore is a minimal store.Store stand-in implementing only the
// methods the stuck-run reaper uses.
type fakeReaperStore struct {
	store.Store

	lockAcquired   bool
	stuck          []string
	listErr        error
	finishedFailed []string
}

func (f *fakeReaperStore) AcquireAdvisoryLock(ctx context.Context, key int64) (func(), error) {
	if !f.lockAcquired {
		return nil, nil
	}
	return func() {}, nil
}

func (f *fakeReaperStore) ListStuckRunIDs(ctx context.Context, staleAfter, grace time.Duration) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.stuck, nil
}

func (f *fakeReaperStore) MarkRunFinished(ctx context.Context, runID string, status api.RunStatus) error {
	if status == api.RunFailed {
		f.finishedFailed = append(f.finishedFailed, runID)
	}
	return nil
}

func TestStuckRunReaper_FailsStuckRunsAsLeader(t *testing.T) {
	st := &fakeReaperStore{
		lockAcquired: true,
		stuck:        []string{"r1", "r2"},
	}
	runStuckRunReaperOnce(context.Background(), st, 90*time.Second, 60*time.Second)
	assert.ElementsMatch(t, []string{"r1", "r2"}, st.finishedFailed)
}

func TestStuckRunReaper_FollowerDoesNothing(t *testing.T) {
	st := &fakeReaperStore{lockAcquired: false, stuck: []string{"r1"}}
	runStuckRunReaperOnce(context.Background(), st, 90*time.Second, 60*time.Second)
	assert.Empty(t, st.finishedFailed)
}
