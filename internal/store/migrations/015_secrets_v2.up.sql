-- Migration 001 owns both objects touched here.
--
-- controller_key_hex stored the KEK in the same database as the wrapped DEKs
-- and ciphertext it protects, so a database dump contained both the ciphertext
-- and the key for it. The key now comes from a file or a KMS only.
--
-- sessions.refresh_token stored the OIDC refresh token as plaintext. It is
-- replaced by envelope-encrypted columns. Existing rows cannot be re-encrypted
-- (their format predates the binding), so sessions are cleared and users log in
-- again — the correct handling for tokens that were stored in the clear.
ALTER TABLE public.controller_settings DROP COLUMN IF EXISTS controller_key_hex;

DELETE FROM public.sessions;

ALTER TABLE public.sessions DROP COLUMN IF EXISTS refresh_token;
ALTER TABLE public.sessions ADD COLUMN IF NOT EXISTS refresh_token_dek bytea NOT NULL DEFAULT ''::bytea;
ALTER TABLE public.sessions ADD COLUMN IF NOT EXISTS refresh_token_ct bytea NOT NULL DEFAULT ''::bytea;
ALTER TABLE public.sessions ALTER COLUMN refresh_token_dek DROP DEFAULT;
ALTER TABLE public.sessions ALTER COLUMN refresh_token_ct DROP DEFAULT;
