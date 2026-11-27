-- Reverses the schema only. PATs, sessions and audit rows deleted by the up
-- migration are not restored, and the accounts table content is lost.
ALTER TABLE public.controller_settings
    DROP COLUMN IF EXISTS initial_token_hash,
    DROP COLUMN IF EXISTS bootstrap_done_at,
    DROP COLUMN IF EXISTS admin_enabled_until;
DROP INDEX IF EXISTS public.sessions_account_id_idx;
DROP INDEX IF EXISTS public.pats_account_id_idx;
ALTER TABLE public.sessions DROP COLUMN IF EXISTS account_id;
ALTER TABLE public.pats DROP COLUMN IF EXISTS account_id;
DROP TABLE IF EXISTS public.accounts;
