-- Reverses the schema only. Rows deleted by the up migration are not restored
-- and the dropped owner values are lost (the columns come back NULL).
ALTER TABLE public.pats
    ADD COLUMN owner_sub    text,
    ADD COLUMN owner_email  text,
    ADD COLUMN owner_claims text[];
CREATE INDEX pats_owner_sub_idx ON public.pats (owner_sub) WHERE owner_sub IS NOT NULL;

DROP INDEX IF EXISTS public.sessions_account_id_idx;
DROP INDEX IF EXISTS public.pats_account_id_idx;
ALTER TABLE public.sessions ALTER COLUMN account_id DROP NOT NULL;
ALTER TABLE public.pats     ALTER COLUMN account_id DROP NOT NULL;
CREATE INDEX pats_account_id_idx ON public.pats (account_id) WHERE account_id IS NOT NULL;
CREATE INDEX sessions_account_id_idx ON public.sessions (account_id) WHERE account_id IS NOT NULL;
