-- audit_logs.actor is a display name and is NOT unique: a PAT name is chosen
-- by whoever mints the token, so two principals can share it (or imitate the
-- bootstrap token's name). actor_id is the stable, unambiguous identity of the
-- caller ("pat:<pat id>", "oidc:<sub>", "break-glass", "agent:<agent id>") and
-- actor_sub is the OIDC subject behind it when there is one. Both are NULL for
-- rows written before this migration.
ALTER TABLE audit_logs ADD COLUMN actor_id text, ADD COLUMN actor_sub text;
