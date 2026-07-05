DROP INDEX IF EXISTS step_reports_child_run_id_idx;
ALTER TABLE step_reports
  DROP COLUMN IF EXISTS call_job_name,
  DROP COLUMN IF EXISTS child_run_id;
