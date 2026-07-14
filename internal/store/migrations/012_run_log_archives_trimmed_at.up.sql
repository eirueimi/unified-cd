-- trimmed_at records when the run's logs rows were deleted from the DB after
-- archival (tiered log storage). NULL = rows still present. It is the single
-- source of truth for "serve this run's logs from the archive object", which
-- keeps 'genuinely empty logs' distinguishable from 'trimmed'.
ALTER TABLE public.run_log_archives ADD COLUMN IF NOT EXISTS trimmed_at timestamp with time zone;
