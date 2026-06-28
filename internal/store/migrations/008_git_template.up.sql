CREATE TABLE git_credentials (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    host        TEXT NOT NULL,
    cred_type   TEXT NOT NULL CHECK (cred_type IN ('token', 'sshKey')),
    secret_ref  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_git_credentials_host ON git_credentials(host);
