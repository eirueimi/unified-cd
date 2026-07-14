package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
)

// runRetentionLockKey is the advisory lock key for the run retention sweeper.
// Distinct from scheduler(0x65786364), approval(0x61707276), cache(0x63616368),
// logArchiver(0x6C6F6761), appSource(0x61707073), stuckRun(0x7374756B),
// auditRetention(0x61756474).
const runRetentionLockKey = int64(0x7272746E) // 'rrtn'

// runRetentionBatchSize is how many expired runs one sweep fetches at a time.
const runRetentionBatchSize = 100

// RunRunRetention periodically deletes terminal runs older than retentionDays,
// including their object-store data (log archives, artifacts). Leader-elected
// via an advisory lock so only one replica sweeps. retentionDays <= 0 disables
// retention entirely (keep forever). Returns immediately if st is nil.
func RunRunRetention(ctx context.Context, st store.Store, obj objectstore.ObjectStore, interval time.Duration, retentionDays int) {
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
		runRunRetentionOnce(ctx, st, obj, retentionDays)
	}
}

func runRunRetentionOnce(ctx context.Context, st store.Store, obj objectstore.ObjectStore, retentionDays int) {
	release, err := st.AcquireAdvisoryLock(ctx, runRetentionLockKey)
	if err != nil {
		slog.Warn("run retention lock", "error", err)
		return
	}
	if release == nil {
		return // Another replica is leader.
	}
	defer release()

	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	total := 0
	for {
		ids, err := st.ListExpiredRuns(ctx, cutoff, runRetentionBatchSize)
		if err != nil {
			slog.Error("run retention: list expired runs", "error", err)
			return
		}
		if len(ids) == 0 {
			break
		}
		deleted := 0
		for _, id := range ids {
			if err := deleteRunEverywhere(ctx, st, obj, id); err != nil {
				slog.Warn("run retention: delete failed, will retry next tick", "run", id, "error", err)
				continue
			}
			deleted++
		}
		total += deleted
		// Failed runs stay in the (oldest-first) result set, so a batch with
		// no progress means the next fetch would return the same IDs — stop
		// and let the next tick retry. A short batch means we drained the
		// backlog.
		if deleted == 0 || len(ids) < runRetentionBatchSize {
			break
		}
	}
	if total > 0 {
		slog.Info("run retention: deleted expired runs", "count", total, "olderThan", cutoff)
	}
}

// deleteRunEverywhere removes a run's object-store data and then its DB row.
// Object deletion goes first so a surviving DB row always still references
// any surviving objects: a failure leaves the run intact for a later retry,
// never an orphaned object. ObjectStore.Delete is nil for missing keys, so
// retries after a partial failure are idempotent. Both the retention sweeper
// and the manual DELETE /runs/{id} handler use this helper. A nil obj
// (object store not configured) skips object deletion — nothing was ever
// uploaded in such deployments.
func deleteRunEverywhere(ctx context.Context, st store.Store, obj objectstore.ObjectStore, runID string) error {
	if obj != nil {
		arch, err := st.GetLogArchive(ctx, runID)
		if err != nil {
			return fmt.Errorf("get log archive: %w", err)
		}
		if arch != nil {
			if err := obj.Delete(ctx, arch.ObjectKey); err != nil {
				return fmt.Errorf("delete log archive object %s: %w", arch.ObjectKey, err)
			}
		}
		keys, err := obj.List(ctx, "artifacts/"+runID+"/")
		if err != nil {
			return fmt.Errorf("list artifact objects: %w", err)
		}
		for _, key := range keys {
			if err := obj.Delete(ctx, key); err != nil {
				return fmt.Errorf("delete artifact object %s: %w", key, err)
			}
		}
	}
	return st.DeleteRun(ctx, runID)
}
