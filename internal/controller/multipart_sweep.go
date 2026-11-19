package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
)

// multipartSweepLockKey is the advisory-lock key of the abandoned multipart
// upload sweep.
const multipartSweepLockKey = int64(0x6d707274) // 'mprt'

// multipartAbandonAge is how old an in-progress multipart upload must be
// before the sweep aborts it. Presigned part URLs expire after 15 minutes and
// are re-requested as an upload proceeds, so a day-old upload that never
// completed is abandoned.
const multipartAbandonAge = 24 * time.Hour

// abortRunUploadsTimeout bounds the run-end abort.
const abortRunUploadsTimeout = time.Minute

// multipartSweepPrefixes are the key prefixes whose uploads the sweep owns.
var multipartSweepPrefixes = []string{"artifacts/", "caches/"}

// abortRunUploads aborts, in the background, the multipart uploads a finished
// run left in progress: everything under its artifact prefix, and everything
// under its job's cache prefix unless another run of the job is still active
// (runs of one job share the cache prefix). The request context's end does
// not cancel it. Anything it misses is aborted by the periodic sweep.
func (s *Server) abortRunUploads(ctx context.Context, runID, jobName string) {
	if s.presigner == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortRunUploadsTimeout)
		defer cancel()
		s.abortRunUploadsNow(ctx, runID, jobName)
	}()
}

// abortRunUploadsNow is the synchronous body of abortRunUploads.
func (s *Server) abortRunUploadsNow(ctx context.Context, runID, jobName string) {
	if s.presigner == nil || runID == "" {
		return
	}
	prefixes := []string{"artifacts/" + runID + "/"}
	if jobName != "" && !s.otherRunOfJobActive(ctx, jobName, runID) {
		prefixes = append(prefixes, cache.JobPrefix(jobName))
	}
	for _, prefix := range prefixes {
		abortUploads(ctx, s.presigner, prefix, time.Time{})
	}
}

// otherRunOfJobActive reports whether a run of jobName other than runID is
// not yet terminal. A lookup failure counts as active: skipping the cache
// prefix only delays cleanup to the sweep, while aborting a live upload
// fails a running job.
func (s *Server) otherRunOfJobActive(ctx context.Context, jobName, runID string) bool {
	if s.store == nil {
		return true
	}
	runs, err := s.store.ListRunsByJob(ctx, jobName, 100)
	if err != nil {
		slog.Warn("multipart abort: listing the job's runs failed; keeping its cache uploads", "job", jobName, "error", err)
		return true
	}
	for _, r := range runs {
		if r.ID != runID && !isTerminalStatus(string(r.Status)) {
			return true
		}
	}
	return false
}

// abortUploads aborts the uploads under prefix initiated before cutoff (a
// zero cutoff aborts all of them). It returns how many were aborted and how
// many aborts failed.
func abortUploads(ctx context.Context, p objectstore.Presigner, prefix string, cutoff time.Time) (aborted, failed int, err error) {
	uploads, err := p.ListMultipartUploads(ctx, prefix)
	if err != nil {
		slog.Warn("multipart abort: listing uploads failed", "prefix", prefix, "error", err)
		return 0, 0, err
	}
	for _, u := range uploads {
		if !cutoff.IsZero() && !u.Initiated.Before(cutoff) {
			continue
		}
		if err := p.AbortMultipart(ctx, u.Key, u.UploadID); err != nil {
			slog.Warn("multipart abort failed", "key", u.Key, "error", err)
			failed++
			continue
		}
		aborted++
	}
	return aborted, failed, nil
}

// RunMultipartSweep aborts, every interval and on the leader replica only,
// multipart uploads under the artifact and cache prefixes that were initiated
// more than 24 hours ago. It returns when ctx is done.
func RunMultipartSweep(ctx context.Context, st store.Store, p objectstore.Presigner, interval time.Duration) {
	if st == nil || p == nil {
		return
	}
	if interval <= 0 {
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
		metrics.ObservePass("multipart_sweep", func() (int, int, error) {
			return runMultipartSweepAsLeader(ctx, st, p, time.Now())
		})
	}
}

func runMultipartSweepAsLeader(ctx context.Context, st store.Store, p objectstore.Presigner, now time.Time) (int, int, error) {
	release, err := st.AcquireAdvisoryLock(ctx, multipartSweepLockKey)
	if err != nil {
		slog.Warn("multipart sweep lock", "error", err)
		return 0, 0, err
	}
	if release == nil {
		return 0, 0, nil // Another replica is leader.
	}
	defer release()
	cutoff := now.Add(-multipartAbandonAge)
	var total, failedTotal int
	var firstErr error
	for _, prefix := range multipartSweepPrefixes {
		n, failed, err := abortUploads(ctx, p, prefix, cutoff)
		total += n
		failedTotal += failed
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if total > 0 {
		slog.Info("multipart sweep", "aborted", total)
	}
	return total, failedTotal, firstErr
}
