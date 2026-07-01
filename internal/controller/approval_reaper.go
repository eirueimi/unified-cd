package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
)

// approvalReaperLockKey is the advisory lock key used by the approval timeout reaper.
// Distinct from schedulerLockKey (0x65786364), cacheCleanupLockKey (0x63616368),
// logArchiverLockKey (0x6C6F6761), and appSourceReconcilerLockKey (0x61707073).
const approvalReaperLockKey = int64(0x61707276) // 'aprv'

// RunApprovalReaper periodically marks expired Pending approvals as TimedOut in
// the audit table. Even when multiple replicas are running, only one performs
// the work due to the advisory lock. It only updates the approval audit row —
// run status is handled elsewhere (the agent fails the step on timeout).
// Returns immediately if st is nil.
func RunApprovalReaper(ctx context.Context, st store.Store, interval time.Duration) {
	if st == nil {
		return
	}
	if interval == 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		runApprovalReaperAsLeader(ctx, st)
	}
}

func runApprovalReaperAsLeader(ctx context.Context, st store.Store) {
	release, err := st.AcquireAdvisoryLock(ctx, approvalReaperLockKey)
	if err != nil {
		slog.Warn("approval reaper lock", "error", err)
		return
	}
	if release == nil {
		return // Another replica is leader.
	}
	defer release()
	n, err := st.MarkExpiredApprovalsTimedOut(ctx)
	if err != nil {
		slog.Error("approval reaper error", "error", err)
		return
	}
	if n > 0 {
		slog.Info("approval reaper: marked timed-out approvals", "count", n)
	}
}
