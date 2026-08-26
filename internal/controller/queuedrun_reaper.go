package controller

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/eirueimi/unified-cd/internal/store"
)

// queuedRunReaperLockKey is the advisory lock key for the queued-run reaper.
// Distinct from scheduler, approval, cache, logArchiver, appSource, and the
// stuck-run reaper (0x7374756B).
const queuedRunReaperLockKey = int64(0x71756575) // 'queu'

// queuedRunReaperBatch bounds how many unclaimable runs one sweep terminalizes.
// The bound is about STALENESS, not load: every run in a batch is failed using the
// liveness answer computed once at the top of the sweep, so batch size and window
// width are the same number (measured: 4ms for one run, 475ms for 161, ~2.93ms per
// run, previously with no cap of any kind). FinishRunIfStatus makes a stale write
// harmless; this keeps the window small as well. Anything not reached this sweep is
// picked up by the next one 30s later.
const queuedRunReaperBatch = 100

// RunQueuedRunReaper periodically fails runs that have sat Queued past minAge
// and that no live agent (heartbeat within staleAfter) can ever claim, because
// no registered agent's labels satisfy the run's agentSelector — e.g. the agent
// they need has disconnected. This unclaimability check is label-only by
// design: it does not consider agent capabilities, so a run that is
// label-claimable but capability-unschedulable (e.g. a native job when only a
// k8s agent is live) is intentionally left Queued rather than auto-failed by
// this reaper — that case is surfaced instead via the JobDetail unschedulable
// banner. Without this reaper, label-unclaimable runs would stay "in
// progress" forever. Leader-elected via an advisory lock so only one replica
// acts. Returns immediately if st is nil.
func RunQueuedRunReaper(ctx context.Context, st store.Store, interval, minAge, staleAfter time.Duration) {
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
		metrics.ObservePass("queued_run_reaper", func() (int, int, error) { return runQueuedRunReaperOnce(ctx, st, minAge, staleAfter) })
	}
}

// runQueuedRunReaperOnce returns (runs reaped, per-run errors, pass error).
// Reaped runs are the "ok" count because reaping IS this worker's work; a
// rising rate is a fleet problem, not a reaper problem, which is why it is
// counted rather than treated as a failure.
func runQueuedRunReaperOnce(ctx context.Context, st store.Store, minAge, staleAfter time.Duration) (int, int, error) {
	release, err := st.AcquireAdvisoryLock(ctx, queuedRunReaperLockKey)
	if err != nil {
		slog.Warn("queued-run reaper lock", "error", err)
		return 0, 0, err
	}
	if release == nil {
		return 0, 0, nil // Another replica is leader.
	}
	defer release()

	refs, err := st.ListUnclaimableQueuedRuns(ctx, minAge, staleAfter, queuedRunReaperBatch)
	if err != nil {
		slog.Error("queued-run reaper list error", "error", err)
		return 0, 0, err
	}
	failed, superseded, errs := 0, 0, 0
	for _, ref := range refs {
		// Re-validate before writing. The list above is a SNAPSHOT of a liveness
		// question ("can any live agent claim this?"), and the answer flips the
		// instant an agent registers and claims — which is precisely what happens
		// when the outage this reaper exists for ENDS. Terminalizing from the
		// snapshot failed a run 8.0ms after a live, label-matching agent claimed it
		// and 3.0ms after its first step was already recorded Running.
		//
		// FinishRunIfStatus, not MarkRunFinished: the shared CAS refuses only
		// terminal statuses, so it would have overwritten that Running run happily.
		// Requiring the run to still be Queued makes any claim — the only way out of
		// Queued — win the race by construction, and it also withholds the run's
		// mutex / named-lock releases, which would otherwise have been issued
		// unconditionally against a run this reaper did not terminalize.
		updated, err := st.FinishRunIfStatus(ctx, ref.ID, api.RunQueued, api.RunFailed)
		if err != nil {
			slog.Error("queued-run reaper: mark failed", "runId", ref.ID, "error", err)
			errs++
			continue
		}
		if !updated {
			// Left Queued between the list and now — claimed by a returning agent,
			// cancelled, or already failed. Not ours; write nothing at all, not even
			// the reason line (which would otherwise state, on a run that is now
			// executing, that no agent could claim it).
			superseded++
			slog.Info("queued-run reaper: run left the Queued state before it could be failed; skipping", "runId", ref.ID)
			continue
		}
		msg := "run failed: no eligible agent available to claim it"
		if len(ref.AgentSelector) > 0 {
			msg += " (requires agent labels: " + strings.Join(ref.AgentSelector, ", ") + ")"
		}
		// Record why on the run so it is visible in the log view. Written after the
		// terminal write rather than before it, so the reason can never appear on a
		// run this reaper did not actually fail.
		if _, err := st.AppendLog(ctx, ref.ID, systemLogStepIndex, "stderr", time.Now().UTC(), msg); err != nil {
			slog.Warn("queued-run reaper: append reason failed", "runId", ref.ID, "error", err)
		}
		cancelDescendantRuns(ctx, st, ref.ID)
		failed++
		slog.Warn("queued-run reaper: failed unclaimable queued run", "runId", ref.ID, "agentSelector", ref.AgentSelector)
	}
	if failed > 0 {
		slog.Info("queued-run reaper: failed unclaimable queued runs", "count", failed, "superseded", superseded)
	}
	return failed, errs, nil
}
