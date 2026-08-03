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
	stuck             []store.StuckRunRef
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

func (f *fakeReaperStore) ListStuckRuns(ctx context.Context, staleAfter, grace time.Duration) ([]store.StuckRunRef, error) {
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

func staleHeartbeat(ids ...string) []store.StuckRunRef {
	out := make([]store.StuckRunRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, store.StuckRunRef{ID: id})
	}
	return out
}

func TestStuckRunReaper_FailsStuckRunsAsLeader(t *testing.T) {
	st := &fakeReaperStore{
		lockAcquired: true,
		stuck:        staleHeartbeat("r1", "r2"),
	}
	runStuckRunReaperOnce(context.Background(), st, 90*time.Second, 60*time.Second, map[string]time.Time{})
	assert.ElementsMatch(t, []string{"r1", "r2"}, st.finishedFailed)
	// Each reaped run must also have its in-flight steps terminalized, so a
	// Failed orphaned run never leaves a step stuck showing Running.
	assert.ElementsMatch(t, []string{"r1", "r2"}, st.stepsInterrupted)
}

// If step reconciliation fails, the run is NOT marked Failed — so the reaper
// re-lists it next tick and retries, rather than leaving a step stuck Running
// under an already-Failed run the reaper can never re-see.
func TestStuckRunReaper_StepReconcileErrorBlocksRunFail(t *testing.T) {
	st := &fakeReaperStore{lockAcquired: true, stuck: staleHeartbeat("r1"), stepsInterruptErr: assert.AnError}
	runStuckRunReaperOnce(context.Background(), st, 90*time.Second, 60*time.Second, map[string]time.Time{})
	assert.Empty(t, st.finishedFailed)
}

func TestStuckRunReaper_FollowerDoesNothing(t *testing.T) {
	st := &fakeReaperStore{lockAcquired: false, stuck: staleHeartbeat("r1")}
	runStuckRunReaperOnce(context.Background(), st, 90*time.Second, 60*time.Second, map[string]time.Time{})
	assert.Empty(t, st.finishedFailed)
}

// A missing agents row is not evidence of death: it is produced by an ordinary
// duplicate-ID deregistration (any start-before-stop rollout) against a healthy,
// heartbeating agent, and the query has no time component for it. The first sweep
// that sees it must therefore only RECORD the observation.
//
// This is the regression test for the measured defect: a healthy agent's run failed
// at claimed_at + 80.089s because its row happened to be absent for 28.4s.
func TestStuckRunReaper_MissingAgentRowIsNotReapedOnFirstSight(t *testing.T) {
	st := &fakeReaperStore{
		lockAcquired: true,
		stuck:        []store.StuckRunRef{{ID: "r1", AgentMissing: true}},
	}
	missing := map[string]time.Time{}
	runStuckRunReaperOnce(context.Background(), st, 90*time.Second, 60*time.Second, missing)
	assert.Empty(t, st.finishedFailed, "a first observation of an absent agents row must not fail the run")
	assert.Contains(t, missing, "r1", "the observation must be recorded so it can be confirmed later")
}

// The agent answering for itself (heartbeat or claim poll re-inserting the row)
// clears the observation, so the confirmation window restarts rather than
// accumulating across unrelated absences.
func TestStuckRunReaper_MissingAgentRowObservationClearedWhenAgentReturns(t *testing.T) {
	st := &fakeReaperStore{
		lockAcquired: true,
		stuck:        []store.StuckRunRef{{ID: "r1", AgentMissing: true}},
	}
	missing := map[string]time.Time{}
	runStuckRunReaperOnce(context.Background(), st, 90*time.Second, 60*time.Second, missing)

	// The row is back, so the run no longer matches the reaper's predicate at all.
	st.stuck = nil
	runStuckRunReaperOnce(context.Background(), st, 90*time.Second, 60*time.Second, missing)
	assert.NotContains(t, missing, "r1")

	// It goes missing again later: this must be a FRESH observation, not one that
	// inherits the age of the first.
	st.stuck = []store.StuckRunRef{{ID: "r1", AgentMissing: true}}
	runStuckRunReaperOnce(context.Background(), st, 90*time.Second, 60*time.Second, missing)
	assert.Empty(t, st.finishedFailed)
}

// A genuinely dead agent never re-inserts its row, so the observation ages past
// the staleness window and the run is reaped — the branch must still work.
func TestStuckRunReaper_MissingAgentRowReapedOnceConfirmed(t *testing.T) {
	st := &fakeReaperStore{
		lockAcquired: true,
		stuck:        []store.StuckRunRef{{ID: "r1", AgentMissing: true}},
	}
	// Seed an observation older than staleAfter, as a prior sweep would have left.
	missing := map[string]time.Time{"r1": time.Now().Add(-91 * time.Second)}
	runStuckRunReaperOnce(context.Background(), st, 90*time.Second, 60*time.Second, missing)
	assert.Equal(t, []string{"r1"}, st.finishedFailed)
	assert.NotContains(t, missing, "r1", "a reaped run must not stay in the confirmation map")
}

// A stale heartbeat is measured silence and keeps its existing latency: it is acted
// on the first time it is seen, with no confirmation delay.
func TestStuckRunReaper_StaleHeartbeatIsNotDelayed(t *testing.T) {
	st := &fakeReaperStore{
		lockAcquired: true,
		stuck:        []store.StuckRunRef{{ID: "dead", AgentMissing: false}, {ID: "absent", AgentMissing: true}},
	}
	runStuckRunReaperOnce(context.Background(), st, 90*time.Second, 60*time.Second, map[string]time.Time{})
	assert.Equal(t, []string{"dead"}, st.finishedFailed)
}
