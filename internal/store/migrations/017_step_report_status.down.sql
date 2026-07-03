ALTER TABLE step_reports DROP CONSTRAINT step_reports_status_check;
ALTER TABLE step_reports ADD CONSTRAINT step_reports_status_check
    CHECK (status IN ('Running','Succeeded','Failed','Cancelled'));
