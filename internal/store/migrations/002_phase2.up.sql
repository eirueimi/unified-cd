CREATE TABLE mutex_holders (
    mutex_name  TEXT PRIMARY KEY,
    run_id      UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE named_lock_slots (
    pool_name   TEXT NOT NULL,
    slot_id     INT  NOT NULL,
    run_id      UUID REFERENCES runs(id) ON DELETE SET NULL,
    acquired_at TIMESTAMPTZ,
    PRIMARY KEY (pool_name, slot_id)
);

CREATE TABLE step_outputs (
    run_id     UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_index INT  NOT NULL,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL,
    PRIMARY KEY (run_id, step_index, key)
);

CREATE TABLE run_outputs (
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    key    TEXT NOT NULL,
    value  TEXT NOT NULL,
    PRIMARY KEY (run_id, key)
);
