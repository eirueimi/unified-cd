ALTER TABLE app_sources
  ADD COLUMN sync_status text NOT NULL DEFAULT '',
  ADD COLUMN last_error  text NOT NULL DEFAULT '';
