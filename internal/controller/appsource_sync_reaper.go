package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
)

// appSourceSyncReaperLockKey is the advisory lock key for the AppSource sync reaper.
// Distinct from scheduler(0x65786364), approval(0x61707276), cache(0x63616368),
// logArchiver(0x6C6F6761), appSource reconciler(0x61707073), stuckRun(0x7374756B).
const appSourceSyncReaperLockKey = int64(0x73796E63) // 'sync'

// RunAppSourceSyncReaper periodically resets AppSources stuck in "Syncing" longer
// than staleAfter. The manual sync-trigger API (handleSyncAppSource) sets
// sync_status="Syncing" synchronously, decoupled from the actual reconcile on the
// next ticker cycle. If the reconciler panics, the process dies, or leadership
// changes mid-sync, the row can stay "Syncing" forever with no timeout (bug #33).
// This reaper bounds that: it clears last_commit (so shouldSync re-syncs on the
// next reconcile tick) and records a last_error. Leader-elected via an advisory
// lock so only one replica acts. Returns immediately if st is nil.
func RunAppSourceSyncReaper(ctx context.Context, st store.Store, interval, staleAfter time.Duration) {
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
		runAppSourceSyncReaperOnce(ctx, st, staleAfter)
	}
}

func runAppSourceSyncReaperOnce(ctx context.Context, st store.Store, staleAfter time.Duration) {
	release, err := st.AcquireAdvisoryLock(ctx, appSourceSyncReaperLockKey)
	if err != nil {
		slog.Warn("appsource sync reaper lock", "error", err)
		return
	}
	if release == nil {
		return // Another replica is leader.
	}
	defer release()

	n, err := st.ResetStuckSyncingAppSources(ctx, staleAfter)
	if err != nil {
		slog.Error("appsource sync reaper: reset error", "error", err)
		return
	}
	if n > 0 {
		slog.Warn("appsource sync reaper: reset stuck Syncing AppSources", "count", n)
	}
}
