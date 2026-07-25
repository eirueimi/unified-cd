DROP INDEX IF EXISTS idx_runs_queued_detached;
ALTER TABLE runs DROP COLUMN IF EXISTS detached;
