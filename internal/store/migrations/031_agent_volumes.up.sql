CREATE TABLE public.agent_volume_defs (
    name       text        NOT NULL,
    spec       jsonb       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT agent_volume_defs_pkey PRIMARY KEY (name)
);
ALTER TABLE public.agents ADD COLUMN volume_status jsonb;
