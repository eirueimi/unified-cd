package controller

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeQueuedReaperStore is a minimal store.Store stand-in covering only what the
// queued-run reaper calls. stillQueued models the run's status at WRITE time, which
// is the whole point: the reaper's list is a snapshot and the world moves under it.
type fakeQueuedReaperStore struct {
	store.Store

	refs        []store.QueuedRunRef
	stillQueued map[string]bool

	batchLimit int
	failed     []string
	logged     []string
}

func (f *fakeQueuedReaperStore) AcquireAdvisoryLock(ctx context.Context, key int64) (func(), error) {
	return func() {}, nil
}

func (f *fakeQueuedReaperStore) ListUnclaimableQueuedRuns(ctx context.Context, minAge, staleAfter time.Duration, limit int) ([]store.QueuedRunRef, error) {
	f.batchLimit = limit
	return f.refs, nil
}

func (f *fakeQueuedReaperStore) FinishRunIfStatus(ctx context.Context, runID string, expected, status api.RunStatus) (bool, error) {
	if expected != api.RunQueued {
		return false, nil
	}
	if !f.stillQueued[runID] {
		return false, nil
	}
	f.failed = append(f.failed, runID)
	return true, nil
}

func (f *fakeQueuedReaperStore) AppendLog(ctx context.Context, runID string, stepIndex int, stream string, ts time.Time, line string) (int64, error) {
	f.logged = append(f.logged, runID)
	return 0, nil
}

func (f *fakeQueuedReaperStore) ListChildRunIDs(ctx context.Context, parentRunID string) ([]string, error) {
	return nil, nil
}

// The measured defect: a run failed 8.0ms after a live, label-matching agent
// claimed it and 3.0ms after its first step was recorded Running, with the reason
// "no eligible agent available to claim it". The reaper must re-validate at write
// time, and must write NOTHING — not even the reason line — for a run it did not
// terminalize.
func TestQueuedRunReaper_SkipsRunsClaimedSinceTheSnapshot(t *testing.T) {
	st := &fakeQueuedReaperStore{
		refs: []store.QueuedRunRef{
			{ID: "claimed-since", AgentSelector: []string{"kind:linux"}},
			{ID: "still-queued", AgentSelector: []string{"kind:linux"}},
		},
		stillQueued: map[string]bool{"still-queued": true},
	}
	runQueuedRunReaperOnce(context.Background(), st, 30*time.Second, 90*time.Second)

	assert.Equal(t, []string{"still-queued"}, st.failed)
	assert.Equal(t, []string{"still-queued"}, st.logged,
		"a run the reaper did not fail must not be told that no agent could claim it")
}

// The reason line is still written for runs the reaper genuinely fails — the one
// reaper that explains itself to the operator must keep doing so.
func TestQueuedRunReaper_StillExplainsTheRunsItDoesFail(t *testing.T) {
	st := &fakeQueuedReaperStore{
		refs:        []store.QueuedRunRef{{ID: "r1", AgentSelector: []string{"unity"}}},
		stillQueued: map[string]bool{"r1": true},
	}
	runQueuedRunReaperOnce(context.Background(), st, 30*time.Second, 90*time.Second)
	require.Equal(t, []string{"r1"}, st.failed)
	assert.Equal(t, []string{"r1"}, st.logged)
}

// The batch is bounded, so the liveness answer cannot go arbitrarily stale while the
// sweep works through a backlog (measured 475ms for 161 runs, previously uncapped).
func TestQueuedRunReaper_BoundsItsBatch(t *testing.T) {
	st := &fakeQueuedReaperStore{stillQueued: map[string]bool{}}
	runQueuedRunReaperOnce(context.Background(), st, 30*time.Second, 90*time.Second)
	assert.Equal(t, queuedRunReaperBatch, st.batchLimit)
	assert.Positive(t, st.batchLimit)
}
