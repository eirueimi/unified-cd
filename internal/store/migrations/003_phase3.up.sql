CREATE TABLE run_log_archives (
    run_id      UUID PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    object_key  TEXT NOT NULL,
    size_bytes  BIGINT NOT NULL DEFAULT 0,
    archived_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
