-- Restore the migration-001 shape. Sessions are cleared in this direction too:
-- the encrypted columns cannot be rendered back into plaintext without the key,
-- and leaving rows behind would violate the restored NOT NULL.
DELETE FROM public.sessions;

ALTER TABLE public.sessions DROP COLUMN IF EXISTS refresh_token_ct;
ALTER TABLE public.sessions DROP COLUMN IF EXISTS refresh_token_dek;
ALTER TABLE public.sessions ADD COLUMN IF NOT EXISTS refresh_token text NOT NULL DEFAULT '';
ALTER TABLE public.sessions ALTER COLUMN refresh_token DROP DEFAULT;

ALTER TABLE public.controller_settings ADD COLUMN IF NOT EXISTS controller_key_hex text NOT NULL DEFAULT '';
ALTER TABLE public.controller_settings ALTER COLUMN controller_key_hex DROP DEFAULT;
