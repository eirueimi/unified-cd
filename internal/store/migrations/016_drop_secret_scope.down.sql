-- Restore the migration-001 shape. Secrets are cleared in this direction too:
-- the name-only ciphertext cannot be re-bound to a (name, scope, scope_ref)
-- AAD without re-encryption, and leaving rows behind would violate the
-- restored NOT NULL columns.
DELETE FROM public.secrets;

ALTER TABLE public.secrets DROP CONSTRAINT IF EXISTS secrets_name_key;
ALTER TABLE public.secrets ADD COLUMN IF NOT EXISTS scope text NOT NULL DEFAULT 'global'::text;
ALTER TABLE public.secrets ADD COLUMN IF NOT EXISTS scope_ref text NOT NULL DEFAULT ''::text;
ALTER TABLE public.secrets ADD CONSTRAINT secrets_name_scope_scope_ref_key UNIQUE (name, scope, scope_ref);
