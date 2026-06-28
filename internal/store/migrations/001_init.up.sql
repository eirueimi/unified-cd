CREATE TABLE jobs (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    api_version TEXT NOT NULL,
    spec        JSONB NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE runs (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_name   TEXT NOT NULL REFERENCES jobs(name) ON DELETE RESTRICT,
    status     TEXT NOT NULL DEFAULT 'Pending'
               CHECK (status IN ('Pending','Queued','Running','Succeeded','Failed','Cancelled')),
    params     JSONB NOT NULL DEFAULT '{}'::jsonb,
    spec       JSONB NOT NULL,
    claimed_by TEXT,
    claimed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX runs_status_created_idx ON runs(status, created_at);

CREATE TABLE step_reports (
    run_id     UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_index INT  NOT NULL,
    status     TEXT NOT NULL CHECK (status IN ('Running','Succeeded','Failed','Cancelled')),
    exit_code  INT,
    started_at TIMESTAMPTZ,
    ended_at   TIMESTAMPTZ,
    PRIMARY KEY (run_id, step_index)
);

CREATE TABLE logs (
    run_id     UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq        BIGSERIAL,
    step_index INT  NOT NULL,
    stream     TEXT NOT NULL CHECK (stream IN ('stdout','stderr')),
    ts         TIMESTAMPTZ NOT NULL,
    line       TEXT NOT NULL,
    PRIMARY KEY (run_id, seq)
);
CREATE INDEX logs_run_idx ON logs(run_id, seq);

CREATE TABLE agents (
    id            TEXT PRIMARY KEY,
    hostname      TEXT NOT NULL,
    os            TEXT NOT NULL,
    labels        TEXT[] NOT NULL DEFAULT '{}',
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
