DROP TABLE IF EXISTS public.cli_login_codes;
ALTER TABLE public.oidc_states
    DROP COLUMN IF EXISTS cli_confirm_hash,
    DROP COLUMN IF EXISTS cli_challenge,
    DROP COLUMN IF EXISTS cli_redirect;
