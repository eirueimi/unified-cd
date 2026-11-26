ALTER TABLE jobs
    ADD COLUMN labels jsonb,
    ADD COLUMN annotations jsonb;

ALTER TABLE runs
    -- trigger is nullable with no default on purpose: a controller replica that
    -- predates this migration keeps inserting runs during a rolling upgrade and
    -- does not know the column. Such rows stay NULL ("trigger unknown") instead
    -- of being mislabelled 'api'.
    ADD COLUMN trigger text,
    ADD COLUMN job_labels jsonb,
    ADD COLUMN job_annotations jsonb,
    ADD COLUMN scheduled_at timestamptz;

UPDATE runs SET trigger = CASE
    WHEN triggered_by LIKE 'webhook:%'  THEN 'webhook'
    WHEN triggered_by LIKE 'schedule:%' THEN 'schedule'
    WHEN triggered_by LIKE 'replay:%'   THEN 'replay'
    WHEN triggered_by LIKE 'agent:%'    THEN 'call'
    ELSE 'api'
END
WHERE trigger IS NULL;
