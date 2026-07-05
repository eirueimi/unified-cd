package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
)

// auditRetentionLockKey is the advisory lock key for the audit log retention
// cleanup task. Distinct from scheduler(0x65786364), approval(0x61707276),
// cache(0x63616368), logArchiver(0x6C6F6761), appSource(0x61707073),
// stuckRun(0x7374756B).
const auditRetentionLockKey = int64(0x61756474) // 'audt'

// RunAuditRetention periodically deletes audit_logs rows older than
// retentionDays. Leader-elected via an advisory lock so only one replica
// performs the deletion. retentionDays <= 0 disables cleanup entirely
// (keep forever). Returns immediately if st is nil.
func RunAuditRetention(ctx context.Context, st store.Store, interval time.Duration, retentionDays int) {
	if st == nil || retentionDays <= 0 {
		return
	}
	if interval == 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		runAuditRetentionOnce(ctx, st, retentionDays)
	}
}

func runAuditRetentionOnce(ctx context.Context, st store.Store, retentionDays int) {
	release, err := st.AcquireAdvisoryLock(ctx, auditRetentionLockKey)
	if err != nil {
		slog.Warn("audit retention lock", "error", err)
		return
	}
	if release == nil {
		return // Another replica is leader.
	}
	defer release()

	before := time.Now().AddDate(0, 0, -retentionDays)
	n, err := st.DeleteAuditLogsOlderThan(ctx, before)
	if err != nil {
		slog.Error("audit retention delete error", "error", err)
		return
	}
	if n > 0 {
		slog.Info("audit retention: deleted expired audit log rows", "count", n, "olderThan", before)
	}
}
