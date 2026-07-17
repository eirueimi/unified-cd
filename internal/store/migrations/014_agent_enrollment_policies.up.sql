CREATE TABLE agent_enrollment_policies (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,
    provider TEXT NOT NULL,
    provider_config JSONB NOT NULL,
    subject_constraints JSONB NOT NULL,
    agent_id_template TEXT NOT NULL,
    allowed_labels TEXT[] NOT NULL DEFAULT '{}',
    required_labels TEXT[] NOT NULL DEFAULT '{}',
    authorized_capabilities TEXT[] NOT NULL DEFAULT '{}',
    access_token_ttl_seconds BIGINT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
