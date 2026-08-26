CREATE TABLE public.vars (
    name       text        NOT NULL,
    spec       jsonb       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT vars_pkey PRIMARY KEY (name)
);
