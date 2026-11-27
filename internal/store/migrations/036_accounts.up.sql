-- Internal accounts: every principal (human or service) is a row in accounts.
-- PATs and sessions point at their account; ownerless PATs and claim-less
-- sessions cannot be attributed to an account and are removed below.

-- 1. accounts. Human rows are keyed by oidc_sub, service rows by name.
CREATE TABLE public.accounts (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text        NOT NULL CHECK (kind IN ('human', 'service')),
    name         text        NOT NULL DEFAULT '',
    oidc_sub     text        NOT NULL DEFAULT '',
    oidc_issuer  text        NOT NULL DEFAULT '',
    email        text        NOT NULL DEFAULT '',
    claims       text[],
    disabled_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz
);
CREATE UNIQUE INDEX accounts_human_sub_key ON public.accounts (oidc_sub) WHERE kind = 'human';
CREATE UNIQUE INDEX accounts_service_name_key ON public.accounts (name) WHERE kind = 'service';

-- 2. The built-in admin service account.
INSERT INTO public.accounts (kind, name) VALUES ('service', 'admin');

-- 3. Account references.
ALTER TABLE public.pats     ADD COLUMN account_id uuid REFERENCES public.accounts(id) ON DELETE CASCADE;
ALTER TABLE public.sessions ADD COLUMN account_id uuid REFERENCES public.accounts(id) ON DELETE CASCADE;
CREATE INDEX pats_account_id_idx ON public.pats (account_id) WHERE account_id IS NOT NULL;
CREATE INDEX sessions_account_id_idx ON public.sessions (account_id) WHERE account_id IS NOT NULL;

-- 4. One human account per distinct PAT owner and per distinct session sub.
-- Sessions with NULL claims are deleted in step 6, so they do not create
-- accounts. Email and claims come from the most recent session, else the most
-- recent PAT. oidc_issuer stays empty: the controller fills it on next sign-in.
INSERT INTO public.accounts (kind, oidc_sub, email, claims)
SELECT 'human', s.sub,
       COALESCE(ls.email, lp.owner_email, ''),
       CASE WHEN ls.sub IS NOT NULL THEN ls.claims ELSE lp.owner_claims END
FROM (
    SELECT owner_sub AS sub FROM public.pats WHERE owner_sub IS NOT NULL
    UNION
    SELECT sub FROM public.sessions WHERE claims IS NOT NULL
) s
LEFT JOIN LATERAL (
    SELECT sub, email, claims FROM public.sessions
    WHERE sessions.sub = s.sub AND claims IS NOT NULL
    ORDER BY created_at DESC, id DESC LIMIT 1
) ls ON true
LEFT JOIN LATERAL (
    SELECT owner_email, owner_claims FROM public.pats
    WHERE pats.owner_sub = s.sub
    ORDER BY created_at DESC, id DESC LIMIT 1
) lp ON true;

UPDATE public.pats p SET account_id = a.id
FROM public.accounts a
WHERE a.kind = 'human' AND a.oidc_sub = p.owner_sub;

UPDATE public.sessions s SET account_id = a.id
FROM public.accounts a
WHERE a.kind = 'human' AND a.oidc_sub = s.sub AND s.claims IS NOT NULL;

-- 5. Remove ownerless PATs, auditing each one.
INSERT INTO public.audit_logs (actor, actor_id, method, path, action, resource, status, details)
SELECT 'migration', 'migration', 'MIGRATE', '', 'token.delete', name, 200,
       jsonb_build_object(
           'patId', id::text,
           'name', name,
           'role', role,
           'createdBy', COALESCE(created_by, ''),
           'reason', 'ownerless PAT removed by the internal-accounts migration')
FROM public.pats
WHERE owner_sub IS NULL;

DELETE FROM public.pats WHERE owner_sub IS NULL;

-- 6. Sessions without a claim snapshot are pre-matrix sessions: force re-login.
DELETE FROM public.sessions WHERE claims IS NULL;

-- 7. Bootstrap state lives on the controller_settings singleton row.
ALTER TABLE public.controller_settings
    ADD COLUMN initial_token_hash  text,
    ADD COLUMN bootstrap_done_at   timestamptz,
    ADD COLUMN admin_enabled_until timestamptz;
INSERT INTO public.controller_settings (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
