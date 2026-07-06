-- Indexes for the unbounded runs table. Without these, three queries seq-scan
-- (or index-scan-then-sort the majority of) the runs table as it grows:
--   * ListRunsByJob    (WHERE job_name = $1 ORDER BY created_at DESC)
--   * ListRunsByAgent  (WHERE claimed_by = $1 ORDER BY created_at DESC)
--   * ListStuckRunIDs  (WHERE status = 'Running' AND claimed_at < ...)
-- The (col, created_at DESC) composites serve both the filter and the sort in
-- one index; the partial index narrows the reaper straight to stale-claimed
-- Running rows. Plain (non-CONCURRENT) CREATE INDEX because golang-migrate
-- wraps each migration in a transaction; IF NOT EXISTS keeps it idempotent.
CREATE INDEX IF NOT EXISTS runs_job_name_created_idx ON public.runs (job_name, created_at DESC);
CREATE INDEX IF NOT EXISTS runs_claimed_by_created_idx ON public.runs (claimed_by, created_at DESC);
CREATE INDEX IF NOT EXISTS runs_running_claimed_at_idx ON public.runs (claimed_at) WHERE status = 'Running';
