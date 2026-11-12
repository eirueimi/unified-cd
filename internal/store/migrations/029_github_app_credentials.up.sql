ALTER TABLE public.git_credentials DROP CONSTRAINT git_credentials_cred_type_check;
ALTER TABLE public.git_credentials ADD CONSTRAINT git_credentials_cred_type_check
    CHECK (cred_type = ANY (ARRAY['token'::text, 'sshKey'::text, 'githubApp'::text]));
ALTER TABLE public.git_credentials ADD COLUMN controller_use boolean NOT NULL DEFAULT true;
ALTER TABLE public.git_credentials ADD COLUMN config jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE public.audit_logs ADD COLUMN details jsonb;
