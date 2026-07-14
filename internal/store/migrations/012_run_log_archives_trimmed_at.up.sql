-- trimmed_at records when the run's logs rows were deleted from the DB after
-- archival (tiered log storage). NULL = rows still present. It is the single
-- source of truth for "serve this run's logs from the archive object", which
-- keeps 'genuinely empty logs' distinguishable from 'trimmed'.
ALTER TABLE public.run_log_archives ADD COLUMN IF NOT EXISTS trimmed_at timestamp with time zone;

-- line_count and max_seq record exactly how many logs rows (and up to what
-- seq) the archive object actually covers at the time it was written. The
-- trim sweeper compares these against the live logs table before deleting
-- rows, so a run whose logs exceeded the archiver's TailLogs cap, or that
-- received log lines after archival, is never trimmed out from under an
-- incomplete archive. DEFAULT 0 is the safe direction: an existing row with
-- unknown coverage never qualifies for trimming while any logs remain.
ALTER TABLE public.run_log_archives ADD COLUMN IF NOT EXISTS line_count bigint NOT NULL DEFAULT 0;
ALTER TABLE public.run_log_archives ADD COLUMN IF NOT EXISTS max_seq bigint NOT NULL DEFAULT 0;
