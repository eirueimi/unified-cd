ALTER TABLE runs DROP CONSTRAINT runs_job_name_fkey;
ALTER TABLE runs ADD CONSTRAINT runs_job_name_fkey
    FOREIGN KEY (job_name) REFERENCES jobs(name) ON DELETE CASCADE;
