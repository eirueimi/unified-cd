ALTER TABLE runs
    DROP COLUMN IF EXISTS scheduled_at,
    DROP COLUMN IF EXISTS job_annotations,
    DROP COLUMN IF EXISTS job_labels,
    DROP COLUMN IF EXISTS trigger;

ALTER TABLE jobs
    DROP COLUMN IF EXISTS annotations,
    DROP COLUMN IF EXISTS labels;
