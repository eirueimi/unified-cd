ALTER TABLE step_reports
  ADD COLUMN child_run_id uuid,
  ADD COLUMN call_job_name text;
CREATE INDEX step_reports_child_run_id_idx ON step_reports (child_run_id);
