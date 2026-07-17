-- Migration 013 owns this table. Reconcile its interval TTL with the
-- repository's integral-seconds representation without recreating it.
ALTER TABLE public.agent_enrollment_policies ADD COLUMN IF NOT EXISTS access_token_ttl_seconds bigint;

DO $$
DECLARE constraint_name text;
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_schema = 'public' AND table_name = 'agent_enrollment_policies' AND column_name = 'access_token_ttl') THEN
        UPDATE public.agent_enrollment_policies
           SET access_token_ttl_seconds = EXTRACT(EPOCH FROM access_token_ttl)::bigint
         WHERE access_token_ttl_seconds IS NULL;
        FOR constraint_name IN
            SELECT conname FROM pg_constraint
             WHERE conrelid = 'public.agent_enrollment_policies'::regclass
               AND pg_get_constraintdef(oid) ~ '\maccess_token_ttl\M'
        LOOP
            EXECUTE format('ALTER TABLE public.agent_enrollment_policies DROP CONSTRAINT %I', constraint_name);
        END LOOP;
        ALTER TABLE public.agent_enrollment_policies DROP COLUMN access_token_ttl;
    END IF;
    FOR constraint_name IN
        SELECT conname FROM pg_constraint
         WHERE conrelid = 'public.agent_enrollment_policies'::regclass
           AND pg_get_constraintdef(oid) ~ '\maccess_token_ttl_seconds\M'
    LOOP
        EXECUTE format('ALTER TABLE public.agent_enrollment_policies DROP CONSTRAINT %I', constraint_name);
    END LOOP;
END $$;

ALTER TABLE public.agent_enrollment_policies
    ALTER COLUMN access_token_ttl_seconds SET NOT NULL,
    ALTER COLUMN access_token_ttl_seconds DROP DEFAULT,
    ADD CONSTRAINT agent_enrollment_policies_access_token_ttl_seconds_check
        CHECK (access_token_ttl_seconds >= 300 AND access_token_ttl_seconds <= 14400);
