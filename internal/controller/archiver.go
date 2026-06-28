package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/unified-cd/unified-cd/internal/objectstore"
	"github.com/unified-cd/unified-cd/internal/store"
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

	key := fmt.Sprintf("runs/%s/logs.ndjson", runID)
	size := int64(buf.Len())
	if err := obj.Put(ctx, key, &buf, size); err != nil {
		return fmt.Errorf("failed to store object: %w", err)
	}
	if err := st.CreateLogArchive(ctx, runID, key, size); err != nil {
		return fmt.Errorf("failed to create archive record: %w", err)
	}
	slog.Info("archived Run logs", "runId", runID, "key", key, "bytes", size)
	return nil
}
