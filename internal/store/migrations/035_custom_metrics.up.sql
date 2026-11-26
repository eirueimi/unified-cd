CREATE TABLE custom_metric_gauges (
    name        text             NOT NULL,
    labels_hash text             NOT NULL,
    labels      jsonb            NOT NULL DEFAULT '{}'::jsonb,
    help        text             NOT NULL DEFAULT '',
    value       double precision NOT NULL DEFAULT 0,
    updated_at  timestamptz      NOT NULL DEFAULT now(),
    PRIMARY KEY (name, labels_hash)
);

CREATE INDEX custom_metric_gauges_updated_at_idx ON custom_metric_gauges (updated_at);

CREATE TABLE custom_metric_realtime (
    run_id     uuid        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_key   text        NOT NULL DEFAULT '',
    name       text        NOT NULL,
    labels     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    help       text        NOT NULL DEFAULT '',
    started_at timestamptz NOT NULL,
    PRIMARY KEY (run_id, step_key, name)
);
