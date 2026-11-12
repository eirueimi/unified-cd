DELETE FROM public.git_credentials WHERE cred_type = 'githubApp';
ALTER TABLE public.git_credentials DROP CONSTRAINT IF EXISTS git_credentials_cred_type_check;
ALTER TABLE public.git_credentials ADD CONSTRAINT git_credentials_cred_type_check
    CHECK (cred_type = ANY (ARRAY['token'::text, 'sshKey'::text]));
ALTER TABLE public.git_credentials DROP COLUMN IF EXISTS config;
ALTER TABLE public.git_credentials DROP COLUMN IF EXISTS controller_use;
ALTER TABLE public.audit_logs DROP COLUMN IF EXISTS details;
