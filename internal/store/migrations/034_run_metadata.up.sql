ALTER TABLE jobs
    ADD COLUMN labels jsonb,
    ADD COLUMN annotations jsonb;

ALTER TABLE runs
    ADD COLUMN trigger text NOT NULL DEFAULT 'api',
    ADD COLUMN job_labels jsonb,
    ADD COLUMN job_annotations jsonb,
    ADD COLUMN scheduled_at timestamptz;

UPDATE runs SET trigger = CASE
    WHEN triggered_by LIKE 'webhook:%'  THEN 'webhook'
    WHEN triggered_by LIKE 'schedule:%' THEN 'schedule'
    WHEN triggered_by LIKE 'replay:%'   THEN 'replay'
    WHEN triggered_by LIKE 'agent:%'    THEN 'call'
    ELSE 'api'
END;
