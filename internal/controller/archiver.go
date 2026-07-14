package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
)

const logArchiverLockKey = int64(0x6C6F6761) // 'loga'

// RunLogArchiver is a goroutine that periodically archives completed Run logs to the object store.
// Even when multiple replicas are running, only one actually performs the archival due to the advisory lock.
func RunLogArchiver(ctx context.Context, st store.Store, obj objectstore.ObjectStore, interval time.Duration) {
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
		runArchiveAsLeader(ctx, st, obj)
	}
}

func runArchiveAsLeader(ctx context.Context, st store.Store, obj objectstore.ObjectStore) {
	release, err := st.AcquireAdvisoryLock(ctx, logArchiverLockKey)
	if err != nil {
		slog.Warn("log archiver lock", "error", err)
		return
	}
	if release == nil {
		return // Another replica is leader.
	}
	defer release()
	if err := archivePendingLogs(ctx, st, obj); err != nil {
		slog.Error("log archiver error", "error", err)
	}
}

func archivePendingLogs(ctx context.Context, st store.Store, obj objectstore.ObjectStore) error {
	runs, err := st.ListRunsNeedingArchival(ctx, 20)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if err := archiveRunLogs(ctx, st, obj, run.ID); err != nil {
			slog.Error("failed to archive Run logs", "runId", run.ID, "error", err)
		}
	}
	return nil
}

// runLogArchiveKey is the deterministic object-store key for a run's archived
// logs. Both the archiver (writer) and the run-retention deletion helper
// (deleteRunEverywhere in run_retention.go) compute it independently rather
// than only trusting the run_log_archives record, so the deletion helper can
// still find and remove the object even when no record exists (or existed
// only transiently) — see the "archiver race" note there.
func runLogArchiveKey(runID string) string {
	return fmt.Sprintf("runs/%s/logs.ndjson", runID)
}

func archiveRunLogs(ctx context.Context, st store.Store, obj objectstore.ObjectStore, runID string) error {
	lines, err := st.TailLogs(ctx, runID, 0, 1_000_000)
	if err != nil {
		return fmt.Errorf("failed to fetch logs: %w", err)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			return fmt.Errorf("failed to encode log line: %w", err)
		}
	}

	key := runLogArchiveKey(runID)
	size := int64(buf.Len())
	if err := obj.Put(ctx, key, &buf, size); err != nil {
		return fmt.Errorf("failed to store object: %w", err)
	}
	// lineCount/maxSeq record exactly what this archive object covers, so
	// TrimRunLogs can detect a run whose logs exceeded this TailLogs cap or
	// grew after archival, and refuse to trim it (see ErrArchiveIncomplete).
	var maxSeq int64
	if len(lines) > 0 {
		maxSeq = lines[len(lines)-1].Seq
	}
	if err := st.CreateLogArchive(ctx, runID, key, size, int64(len(lines)), maxSeq); err != nil {
		// The object now exists with nothing in the DB pointing at it (e.g.
		// the run was deleted by the retention sweeper between our TailLogs
		// read and this insert, so the FK on run_id fails). Clean up the
		// orphan best-effort rather than leaving it forever.
		if delErr := obj.Delete(ctx, key); delErr != nil {
			slog.Warn("failed to clean up orphaned log archive object after CreateLogArchive failure",
				"runId", runID, "key", key, "error", delErr)
		}
		return fmt.Errorf("failed to create archive record: %w", err)
	}
	slog.Info("archived Run logs", "runId", runID, "key", key, "bytes", size)
	return nil
}
