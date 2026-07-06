package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
)

// stuckRunReaperLockKey is the advisory lock key for the stuck-run reaper.
// Distinct from scheduler(0x65786364), approval(0x61707276), cache(0x63616368),
// logArchiver(0x6C6F6761), appSource(0x61707073).
const stuckRunReaperLockKey = int64(0x7374756B) // 'stuk'

// RunStuckRunReaper periodically fails Running runs whose claiming agent has
// died (no heartbeat within staleAfter, or the agent row is gone), so a run
// never hangs forever on agent loss. Leader-elected via an advisory lock so only
// one replica acts. Fails (never re-queues) — re-running partially-executed steps
// could duplicate side effects. Returns immediately if st is nil.
func RunStuckRunReaper(ctx context.Context, st store.Store, interval, staleAfter, grace time.Duration) {
	if st == nil {
		return
	}
	if interval == 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		runStuckRunReaperOnce(ctx, st, staleAfter, grace)
	}
}

func runStuckRunReaperOnce(ctx context.Context, st store.Store, staleAfter, grace time.Duration) {
	release, err := st.AcquireAdvisoryLock(ctx, stuckRunReaperLockKey)
	if err != nil {
		slog.Warn("stuck-run reaper lock", "error", err)
		return
	}
	if release == nil {
		return // Another replica is leader.
	}
	defer release()

	ids, err := st.ListStuckRunIDs(ctx, staleAfter, grace)
	if err != nil {
		slog.Error("stuck-run reaper list error", "error", err)
		return
	}
	for _, id := range ids {
		if err := failOrphanedRun(ctx, st, id); err != nil {
			slog.Error("stuck-run reaper: mark failed", "runId", id, "error", err)
			continue
		}
		slog.Warn("stuck-run reaper: failed orphaned run (agent lost)", "runId", id)
	}
	if len(ids) > 0 {
		slog.Info("stuck-run reaper: failed orphaned runs", "count", len(ids))
	}
}

// failOrphanedRun marks a run Failed and cascade-cancels its call: descendants
// — the shared semantics for a run whose executing agent process is gone.
// Used by the stuck-run reaper (heartbeat loss / agent deleted) and the agent
// reconcile endpoint (restart with the same ID / force shutdown). Failed, not
// re-queued: re-running partially-executed steps could duplicate side effects.
// MarkRunFinished also releases the run's mutex/semaphore locks, so it must be
// called per-run rather than via a bulk UPDATE.
func failOrphanedRun(ctx context.Context, st store.Store, runID string) error {
	if err := st.MarkRunFinished(ctx, runID, api.RunFailed); err != nil {
		return err
	}
	// An orphaned parent should not leave its call: children running/queued.
	cancelDescendantRuns(ctx, st, runID)
	return nil
}
