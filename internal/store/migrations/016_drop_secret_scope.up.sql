-- Migration 001 owns the secrets table. The scope / scope_ref columns
-- supported per-scope secret values, but the agent fetch path always read
-- scope='global' (internal/controller/api_secrets.go), so a non-global secret
-- could be written and never read — a half-implemented feature. Collapse to a
-- name-only model.
--
-- SecretBinding's canonical encoding is AES-GCM additional authenticated data;
-- dropping scope/scope_ref from it changes the AAD, so every existing
-- ciphertext (global rows included) can no longer be authenticated. There are
-- no production secrets to preserve, so the rows are deleted and operators
-- re-set their secrets — the same handling migration 015 used for
-- sessions.refresh_token.
DELETE FROM public.secrets;

ALTER TABLE public.secrets DROP CONSTRAINT IF EXISTS secrets_name_scope_scope_ref_key;
ALTER TABLE public.secrets DROP COLUMN IF EXISTS scope;
ALTER TABLE public.secrets DROP COLUMN IF EXISTS scope_ref;
ALTER TABLE public.secrets ADD CONSTRAINT secrets_name_key UNIQUE (name);
