ALTER TABLE public.run_log_archives DROP COLUMN IF EXISTS max_seq;
ALTER TABLE public.run_log_archives DROP COLUMN IF EXISTS line_count;
ALTER TABLE public.run_log_archives DROP COLUMN IF EXISTS trimmed_at;
