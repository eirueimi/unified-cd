package controller

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
)

// logTrimLockKey is the advisory lock key for the log-trim sweeper. Distinct
// from scheduler(0x65786364), approval(0x61707276), cache(0x63616368),
// logArchiver(0x6C6F6761), appSource(0x61707073), stuckRun(0x7374756B),
// auditRetention(0x61756474), runRetention(0x7272746E).
const logTrimLockKey = int64(0x6C74726D) // 'ltrm'

// logTrimBatchSize is how many trim candidates one sweep fetches at a time.
const logTrimBatchSize = 100

// RunLogTrim periodically deletes the DB logs rows of runs whose logs were
// archived more than trimDays ago, marking run_log_archives.trimmed_at so
// reads switch to the archive object (tiered log storage). Leader-elected via
// an advisory lock. trimDays <= 0 disables trimming; a nil object store also
// disables it (nothing was ever archived, and trimming would destroy the only
// copy). Returns immediately if st is nil.
func RunLogTrim(ctx context.Context, st store.Store, obj objectstore.ObjectStore, interval time.Duration, trimDays int) {
	if st == nil || obj == nil || trimDays <= 0 {
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
		runLogTrimOnce(ctx, st, obj, trimDays)
	}
}

func runLogTrimOnce(ctx context.Context, st store.Store, obj objectstore.ObjectStore, trimDays int) {
	release, err := st.AcquireAdvisoryLock(ctx, logTrimLockKey)
	if err != nil {
		slog.Warn("log trim lock", "error", err)
		return
	}
	if release == nil {
		return // Another replica is leader.
	}
	defer release()

	cutoff := time.Now().AddDate(0, 0, -trimDays)
	totalRuns := 0
	for {
		ids, err := st.ListTrimCandidates(ctx, cutoff, logTrimBatchSize)
		if err != nil {
			slog.Error("log trim: list candidates", "error", err)
			return
		}
		if len(ids) == 0 {
			break
		}
		progressed := 0
		for _, id := range ids {
			if ctx.Err() != nil {
				return // shutting down; the next leader resumes
			}
			// Trimming is irreversible: never trust the DB record alone,
			// verify the archive object actually exists first.
			keys, err := obj.List(ctx, runLogArchiveKey(id))
			if err != nil {
				slog.Warn("log trim: verify archive object", "run", id, "error", err)
				continue
			}
			if len(keys) == 0 {
				// Stale record with no object (e.g. bucket tampering).
				// Delete the record so ListRunsNeedingArchival picks the run
				// up again and the archiver re-creates the archive; trimming
				// then happens on a later sweep.
				slog.Warn("log trim: archive object missing, deleting stale record for re-archival", "run", id)
				if err := st.DeleteLogArchive(ctx, id); err != nil {
					slog.Warn("log trim: delete stale archive record", "run", id, "error", err)
					continue
				}
				progressed++ // the candidate left the result set
				continue
			}
			n, err := st.TrimRunLogs(ctx, id)
			if err != nil {
				if errors.Is(err, store.ErrArchiveIncomplete) {
					// The archive doesn't (yet) cover all of this run's
					// logs. Two cases land here, distinguished by fetching
					// the record: a legacy record from before this branch's
					// archiver recorded coverage (line_count = max_seq = 0,
					// migration 012's default) has genuinely unknown
					// coverage rather than known-incomplete coverage — delete
					// it so ListRunsNeedingArchival re-archives the run with
					// real counts, and count it as progress so the sweep
					// doesn't wedge on it forever (ListTrimCandidates keeps
					// re-offering it otherwise, since it's oldest-first). A
					// non-legacy record means the run's logs genuinely
					// exceed the archiver's TailLogs cap, or lines were
					// appended after archival: warn and skip with no
					// progress and no deletion, same as before — deleting
					// that record would make the archiver retry forever
					// without ever producing complete coverage.
					arch, gErr := st.GetLogArchive(ctx, id)
					if gErr == nil && arch != nil && arch.LineCount == 0 && arch.MaxSeq == 0 {
						slog.Warn("log trim: legacy archive record without coverage, deleting so the archiver re-archives", "run", id)
						if dErr := st.DeleteLogArchive(ctx, id); dErr != nil {
							slog.Warn("log trim: delete legacy archive record", "run", id, "error", dErr)
							continue
						}
						progressed++
						continue
					}
					if gErr != nil {
						slog.Warn("log trim: fetch archive record for incomplete-coverage run", "run", id, "error", gErr)
					}
					slog.Warn("log trim: archive coverage incomplete, skipping until archiver catches up", "run", id)
				} else {
					slog.Warn("log trim: trim failed, will retry next tick", "run", id, "error", err)
				}
				continue
			}
			progressed++
			totalRuns++
			slog.Debug("log trim: trimmed run logs", "run", id, "rows", n)
		}
		// Candidates that failed stay in the (oldest-first) result set, so a
		// batch with no progress means the next fetch would return the same
		// IDs — stop and let the next tick retry.
		if progressed == 0 || len(ids) < logTrimBatchSize {
			break
		}
	}
	if totalRuns > 0 {
		slog.Info("log trim: trimmed archived runs' DB log rows", "runs", totalRuns, "olderThan", cutoff)
	}
}
