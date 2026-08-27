-- display_name stores the run's interpolated, sanitized, length-capped
-- display name (see internal/controller/run_display_name.go). NULL --
-- not empty string -- means "no displayName: declared on the job", which
-- is every run created before this migration and every run whose job
-- doesn't opt in; the WebUI falls back to the truncated run ID in that
-- case, so leaving this column NULL must not change any existing run's
-- rendering.
ALTER TABLE runs ADD COLUMN display_name text;
