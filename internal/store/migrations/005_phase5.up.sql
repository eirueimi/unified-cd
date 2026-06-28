CREATE TABLE secrets (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL,
    scope         TEXT NOT NULL DEFAULT 'global',
    scope_ref     TEXT NOT NULL DEFAULT '',
    encrypted_dek BYTEA NOT NULL,
    ciphertext    BYTEA NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(name, scope, scope_ref)
);
