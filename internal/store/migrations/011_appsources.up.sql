CREATE TABLE app_sources (
    name          TEXT PRIMARY KEY,
    spec          JSONB NOT NULL,
    last_synced_at TIMESTAMPTZ,
    last_commit   TEXT NOT NULL DEFAULT '',
    managed_jobs  TEXT[] NOT NULL DEFAULT '{}',
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
