CREATE TABLE schedules (
    name          TEXT PRIMARY KEY,
    cron          TEXT NOT NULL,
    job_name      TEXT NOT NULL REFERENCES jobs(name) ON DELETE CASCADE,
    params        JSONB NOT NULL DEFAULT '{}',
    last_fired_at TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_schedules_job_name ON schedules(job_name);
