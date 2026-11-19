-- Browser login for the CLI: the OIDC state remembers where the callback must
-- hand the one-time code (a loopback redirect) and the PKCE challenge it is
-- bound to; cli_login_codes holds the encrypted tokens until the CLI redeems
-- the code. Only the SHA-256 of the code is stored.
ALTER TABLE public.oidc_states
    ADD COLUMN cli_redirect text NOT NULL DEFAULT '',
    ADD COLUMN cli_challenge text NOT NULL DEFAULT '';

CREATE TABLE public.cli_login_codes (
    id uuid DEFAULT gen_random_uuid() PRIMARY KEY,
    code_hash text NOT NULL UNIQUE,
    challenge text NOT NULL,
    tokens_dek bytea NOT NULL,
    tokens_ct bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);
