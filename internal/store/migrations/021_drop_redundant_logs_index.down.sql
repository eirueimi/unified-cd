-- Recreate the index this migration dropped, matching 001_init.up.sql's
-- original definition exactly.
CREATE INDEX IF NOT EXISTS logs_run_idx ON public.logs USING btree (run_id, seq);
