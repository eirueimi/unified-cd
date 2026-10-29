-- Matrix authorization data model (see
-- docs/superpowers/specs/2026-10-28-matrix-authorization-design.md, section 7.5).
--
-- One forward migration, no backfill. Existing rows keep behaving exactly as
-- before because the new columns are all nullable and NULL means "legacy":
--   * pats.owner_sub IS NULL  <=>  role-only PAT (created before this change,
--     or the UNIFIED_TOKEN bootstrap row). Its role column is its whole
--     authority. A PAT with an owner records the OIDC identity (sub, email and
--     the claim values at creation) that its permissions are bounded by.
--   * pats.permissions is the optional ceiling list; NULL means the role
--     column is the ceiling (or no extra ceiling when an owner exists and the
--     role is empty).
--   * sessions.claims IS NULL  <=>  a session created before this change; it
--     is role-only and cannot mint group-aware tokens until the user logs in
--     again.
--   * audit_logs.permission / scope / granted_by record which permission
--     allowed a request, where, and through which grant. NULL for rows
--     written before this change and for requests that needed no decision.
--
-- pats.role and sessions.role lose their DEFAULT 'admin' and stay NOT NULL:
-- every writer must now state a role explicitly (an empty string is allowed
-- only together with an owner or claims), so a forgotten argument can no
-- longer silently produce an admin.
ALTER TABLE pats ADD COLUMN owner_sub text, ADD COLUMN owner_email text,
  ADD COLUMN owner_claims text[], ADD COLUMN permissions text[];
ALTER TABLE pats ALTER COLUMN role DROP DEFAULT;
CREATE INDEX pats_owner_sub_idx ON pats (owner_sub) WHERE owner_sub IS NOT NULL;
ALTER TABLE sessions ADD COLUMN claims text[];
ALTER TABLE sessions ALTER COLUMN role DROP DEFAULT;
ALTER TABLE audit_logs ADD COLUMN permission text, ADD COLUMN scope text, ADD COLUMN granted_by text;
