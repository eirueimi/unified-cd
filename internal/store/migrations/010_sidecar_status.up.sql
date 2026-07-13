CREATE TABLE public.sidecar_status (
    run_id     uuid    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    idx        integer NOT NULL,
    name       text    NOT NULL,
    phase      text    NOT NULL,
    exit_code  integer,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, idx)
);
