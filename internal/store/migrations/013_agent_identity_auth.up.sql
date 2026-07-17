CREATE TABLE public.agent_identities (
  id uuid DEFAULT gen_random_uuid() PRIMARY KEY,
  agent_id text NOT NULL UNIQUE,
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  enrollment_method text NOT NULL,
  external_subject text,
  authorized_labels text[] NOT NULL DEFAULT '{}',
  authorized_capabilities text[] NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now(),
  disabled_at timestamptz,
  last_authenticated_at timestamptz
);

CREATE UNIQUE INDEX agent_identities_external_subject_idx
  ON public.agent_identities(enrollment_method, external_subject)
  WHERE external_subject IS NOT NULL;

CREATE TABLE public.agent_credentials (
  id uuid PRIMARY KEY,
  identity_id uuid NOT NULL REFERENCES public.agent_identities(id) ON DELETE CASCADE,
  kind text NOT NULL CHECK (kind IN ('access','refresh')),
  family_id uuid,
  generation integer NOT NULL DEFAULT 0 CHECK (generation >= 0),
  token_hash text NOT NULL UNIQUE CHECK (length(token_hash)=64),
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz,
  replaced_by uuid REFERENCES public.agent_credentials(id),
  superseded_at timestamptz,
  overlap_expires_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz
);

CREATE INDEX agent_credentials_identity_idx ON public.agent_credentials(identity_id);
CREATE INDEX agent_credentials_family_idx ON public.agent_credentials(family_id,generation);

CREATE TABLE public.agent_enrollment_tokens (
  id uuid PRIMARY KEY,
  agent_id text NOT NULL,
  token_hash text NOT NULL UNIQUE CHECK (length(token_hash)=64),
  authorized_labels text[] NOT NULL DEFAULT '{}',
  authorized_capabilities text[] NOT NULL DEFAULT '{}',
  expires_at timestamptz NOT NULL,
  used_at timestamptz,
  revoked_at timestamptz,
  created_by text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE public.agent_enrollment_policies (
  id uuid DEFAULT gen_random_uuid() PRIMARY KEY,
  name text NOT NULL UNIQUE,
  provider text NOT NULL CHECK (provider='kubernetes'),
  provider_config jsonb NOT NULL DEFAULT '{}',
  subject_constraints jsonb NOT NULL DEFAULT '{}',
  agent_id_template text NOT NULL,
  allowed_labels text[] NOT NULL DEFAULT '{}',
  required_labels text[] NOT NULL DEFAULT '{}',
  authorized_capabilities text[] NOT NULL DEFAULT '{}',
  access_token_ttl interval NOT NULL DEFAULT interval '1 hour',
  enabled boolean NOT NULL DEFAULT true,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK (access_token_ttl >= interval '5 minutes' AND access_token_ttl <= interval '24 hours')
);
