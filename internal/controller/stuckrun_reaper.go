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
	// missingSince tracks, per run, when this leader FIRST saw the run's claiming
	// agent missing from the inventory. See runStuckRunReaperOnce.
	missingSince := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		runStuckRunReaperOnce(ctx, st, staleAfter, grace, missingSince)
	}
}

// runStuckRunReaperOnce fails one sweep's worth of orphaned runs.
//
// The two ways a run can match are NOT equally trustworthy, and the difference is
// the whole reason for missingSince:
//
//   - a STALE HEARTBEAT is measured silence. The agent had a row, stopped writing
//     to it, and staleAfter has elapsed. Act immediately, as before.
//   - a MISSING ROW is not evidence of anything. The `agents` row is not owned by
//     the process that heartbeats into it: any process holding that agent ID's
//     credential can delete it, and `Agent.Run` deregisters unconditionally at the
//     end of drain — so an ordinary start-before-stop rollout under one agent ID
//     deletes a row a healthy, heartbeating process is using. The predicate has no
//     time component for this branch (an absent row has no timestamp to age), so
//     before this confirmation a healthy agent's run was failed as "agent lost"
//     purely because its row happened to be absent at sweep time. Measured end to
//     end with a plain `docker compose stop` of a duplicate-ID twin: 28.4s of row
//     absence, run failed at claimed_at + 80.089s.
//
// So a missing row must be observed CONTINUOUSLY for staleAfter before it is acted
// on. TouchAgent recreates the row on the next heartbeat (≤ one heartbeat interval),
// which clears the observation; a genuinely dead agent never clears it. The state is
// leader-local and in-memory on purpose: it needs no schema, and losing it on
// leadership change only DELAYS a reap by one confirmation window, which is the safe
// direction. Note this branch is not the one that handles ordinary agent death —
// that is the stale-heartbeat branch at staleAfter — it only handles rows that
// DeleteStaleAgents removed, five minutes after the run was already reaped.
func runStuckRunReaperOnce(ctx context.Context, st store.Store, staleAfter, grace time.Duration, missingSince map[string]time.Time) {
	release, err := st.AcquireAdvisoryLock(ctx, stuckRunReaperLockKey)
	if err != nil {
		slog.Warn("stuck-run reaper lock", "error", err)
		return
	}
	if release == nil {
		return // Another replica is leader.
	}
	defer release()

	refs, err := st.ListStuckRuns(ctx, staleAfter, grace)
	if err != nil {
		slog.Error("stuck-run reaper list error", "error", err)
		return
	}

	now := time.Now()
	// Forget runs that are no longer listed with a missing agent row — either the
	// agent came back (its heartbeat or claim poll re-inserted the row) or the run
	// left the Running state. Either way the earlier observation is void and must
	// not accumulate toward a future reap.
	stillMissing := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref.AgentMissing {
			stillMissing[ref.ID] = struct{}{}
		}
	}
	for id := range missingSince {
		if _, ok := stillMissing[id]; !ok {
			delete(missingSince, id)
		}
	}

	failed := 0
	for _, ref := range refs {
		if ref.AgentMissing {
			first, seen := missingSince[ref.ID]
			if !seen {
				missingSince[ref.ID] = now
				slog.Info("stuck-run reaper: claiming agent has no inventory row; confirming before reaping",
					"runId", ref.ID, "confirmAfter", staleAfter.String())
				continue
			}
			if now.Sub(first) < staleAfter {
				continue // Not yet confirmed; re-check next sweep.
			}
		}
		if err := failOrphanedRun(ctx, st, ref.ID); err != nil {
			slog.Error("stuck-run reaper: mark failed", "runId", ref.ID, "error", err)
			continue
		}
		delete(missingSince, ref.ID)
		failed++
		reason := "agent heartbeat stale"
		if ref.AgentMissing {
			reason = "agent inventory row absent for longer than the staleness window"
		}
		slog.Warn("stuck-run reaper: failed orphaned run (agent lost)", "runId", ref.ID, "reason", reason)
	}
	if failed > 0 {
		slog.Info("stuck-run reaper: failed orphaned runs", "count", failed)
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
	// Terminalize the run's in-flight steps BEFORE failing the run, while the
	// run is still listable by the reaper: a transient error here is then
	// retried on the next tick instead of leaving a step stuck showing Running
	// under an already-Failed run (which the reaper would never re-list).
	if _, err := st.MarkRunStepsInterrupted(ctx, runID); err != nil {
		return err
	}
	if err := st.MarkRunFinished(ctx, runID, api.RunFailed); err != nil {
		return err
	}
	// An orphaned parent should not leave its call: children running/queued.
	cancelDescendantRuns(ctx, st, runID)
	return nil
}
