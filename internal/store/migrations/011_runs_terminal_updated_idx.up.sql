-- Supports ListExpiredRuns, which the run-retention sweeper calls every
-- batch (100 rows) on every tick:
--   WHERE status IN ('Succeeded', 'Failed', 'Cancelled') AND updated_at < $1
--   ORDER BY updated_at LIMIT $2
-- Without this index the query sequentially scans the whole (unbounded)
-- runs table. The partial index narrows it to exactly the terminal
-- statuses the sweeper cares about, and (updated_at) alone serves both the
-- filter and the ORDER BY. Plain (non-CONCURRENT) CREATE INDEX because
-- golang-migrate wraps each migration in a transaction; IF NOT EXISTS keeps
-- it idempotent.
CREATE INDEX IF NOT EXISTS runs_terminal_updated_idx ON public.runs (updated_at)
	WHERE status IN ('Succeeded', 'Failed', 'Cancelled');
