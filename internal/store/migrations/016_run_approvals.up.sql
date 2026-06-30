CREATE TABLE run_approvals (
    run_id      UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_index  INT  NOT NULL,
    step_name   TEXT NOT NULL,
    message     TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL,
    decided_by  TEXT,
    comment     TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    timeout_at  TIMESTAMPTZ,
    decided_at  TIMESTAMPTZ,
    PRIMARY KEY (run_id, step_index)
);
