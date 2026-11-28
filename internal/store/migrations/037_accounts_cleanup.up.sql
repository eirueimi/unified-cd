-- Internal accounts cleanup: every PAT and session now belongs to an account,
-- and the per-PAT owner snapshot (owner_sub, owner_email, owner_claims) is
-- replaced by the accounts row.
--
-- sessions.sub, sessions.email and sessions.claims are kept on purpose: the
-- OIDC refresh path still reads them.

-- Migration 036 attributed or deleted every pre-existing row. Remove any row
-- written without an account since then (by an older replica) so that the
-- NOT NULL constraints below cannot fail.
DELETE FROM public.pats WHERE account_id IS NULL;
DELETE FROM public.sessions WHERE account_id IS NULL;

ALTER TABLE public.pats     ALTER COLUMN account_id SET NOT NULL;
ALTER TABLE public.sessions ALTER COLUMN account_id SET NOT NULL;

-- The partial account_id indexes are redundant now that the column is NOT NULL
-- and fully indexed by the full indexes below.
DROP INDEX IF EXISTS public.pats_account_id_idx;
DROP INDEX IF EXISTS public.sessions_account_id_idx;
CREATE INDEX pats_account_id_idx ON public.pats (account_id);
CREATE INDEX sessions_account_id_idx ON public.sessions (account_id);

DROP INDEX IF EXISTS public.pats_owner_sub_idx;
ALTER TABLE public.pats
    DROP COLUMN owner_sub,
    DROP COLUMN owner_email,
    DROP COLUMN owner_claims;
