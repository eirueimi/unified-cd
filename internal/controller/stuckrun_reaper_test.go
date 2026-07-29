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

	lockAcquired      bool
	stuck             []string
	listErr           error
	finishedFailed    []string
	stepsInterrupted  []string
	stepsInterruptErr error
}

func (f *fakeReaperStore) MarkRunStepsInterrupted(ctx context.Context, runID string) (int64, error) {
	if f.stepsInterruptErr != nil {
		return 0, f.stepsInterruptErr
	}
	f.stepsInterrupted = append(f.stepsInterrupted, runID)
	return 0, nil
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

// ListChildRunIDs is called by the cascade-cancel the reaper now runs after
// failing a run; this fake has no children.
func (f *fakeReaperStore) ListChildRunIDs(ctx context.Context, parentRunID string) ([]string, error) {
	return nil, nil
}

func TestStuckRunReaper_FailsStuckRunsAsLeader(t *testing.T) {
	st := &fakeReaperStore{
		lockAcquired: true,
		stuck:        []string{"r1", "r2"},
	}
	runStuckRunReaperOnce(context.Background(), st, 90*time.Second, 60*time.Second)
	assert.ElementsMatch(t, []string{"r1", "r2"}, st.finishedFailed)
	// Each reaped run must also have its in-flight steps terminalized, so a
	// Failed orphaned run never leaves a step stuck showing Running.
	assert.ElementsMatch(t, []string{"r1", "r2"}, st.stepsInterrupted)
}

// If step reconciliation fails, the run is NOT marked Failed — so the reaper
// re-lists it next tick and retries, rather than leaving a step stuck Running
// under an already-Failed run the reaper can never re-see.
func TestStuckRunReaper_StepReconcileErrorBlocksRunFail(t *testing.T) {
	st := &fakeReaperStore{lockAcquired: true, stuck: []string{"r1"}, stepsInterruptErr: assert.AnError}
	runStuckRunReaperOnce(context.Background(), st, 90*time.Second, 60*time.Second)
	assert.Empty(t, st.finishedFailed)
}

func TestStuckRunReaper_FollowerDoesNothing(t *testing.T) {
	st := &fakeReaperStore{lockAcquired: false, stuck: []string{"r1"}}
	runStuckRunReaperOnce(context.Background(), st, 90*time.Second, 60*time.Second)
	assert.Empty(t, st.finishedFailed)
}
