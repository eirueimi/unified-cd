package controller

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
)

// queuedRunReaperLockKey is the advisory lock key for the queued-run reaper.
// Distinct from scheduler, approval, cache, logArchiver, appSource, and the
// stuck-run reaper (0x7374756B).
const queuedRunReaperLockKey = int64(0x71756575) // 'queu'

// RunQueuedRunReaper periodically fails runs that have sat Queued past minAge
// and that no live agent (heartbeat within staleAfter) can ever claim, because
// no registered agent's labels satisfy the run's agentSelector — e.g. the agent
// they need has disconnected. Without this, such runs stay "in progress" forever.
// Leader-elected via an advisory lock so only one replica acts. Returns
// immediately if st is nil.
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
		runQueuedRunReaperOnce(ctx, st, minAge, staleAfter)
	}
}

func runQueuedRunReaperOnce(ctx context.Context, st store.Store, minAge, staleAfter time.Duration) {
	release, err := st.AcquireAdvisoryLock(ctx, queuedRunReaperLockKey)
	if err != nil {
		slog.Warn("queued-run reaper lock", "error", err)
		return
	}
	if release == nil {
		return // Another replica is leader.
	}
	defer release()

	refs, err := st.ListUnclaimableQueuedRuns(ctx, minAge, staleAfter)
	if err != nil {
		slog.Error("queued-run reaper list error", "error", err)
		return
	}
	for _, ref := range refs {
		msg := "run failed: no eligible agent available to claim it"
		if len(ref.AgentSelector) > 0 {
			msg += " (requires agent labels: " + strings.Join(ref.AgentSelector, ", ") + ")"
		}
		// Record why on the run so it is visible in the log view, before failing.
		if _, err := st.AppendLog(ctx, ref.ID, -1, "stderr", time.Now().UTC(), msg); err != nil {
			slog.Warn("queued-run reaper: append reason failed", "runId", ref.ID, "error", err)
		}
		if err := st.MarkRunFinished(ctx, ref.ID, api.RunFailed); err != nil {
			slog.Error("queued-run reaper: mark failed", "runId", ref.ID, "error", err)
			continue
		}
		cancelDescendantRuns(ctx, st, ref.ID)
		slog.Warn("queued-run reaper: failed unclaimable queued run", "runId", ref.ID, "agentSelector", ref.AgentSelector)
	}
	if len(refs) > 0 {
		slog.Info("queued-run reaper: failed unclaimable queued runs", "count", len(refs))
	}
}
