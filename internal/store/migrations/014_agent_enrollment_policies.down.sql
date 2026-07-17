-- Restore the migration-013-owned interval column without dropping policy data.
ALTER TABLE public.agent_enrollment_policies ADD COLUMN IF NOT EXISTS access_token_ttl interval;
UPDATE public.agent_enrollment_policies SET access_token_ttl = make_interval(secs => access_token_ttl_seconds) WHERE access_token_ttl IS NULL;
DO $$
DECLARE constraint_name text;
BEGIN
    FOR constraint_name IN
        SELECT conname FROM pg_constraint
         WHERE conrelid = 'public.agent_enrollment_policies'::regclass
           AND pg_get_constraintdef(oid) ~ '\maccess_token_ttl_seconds\M'
    LOOP
        EXECUTE format('ALTER TABLE public.agent_enrollment_policies DROP CONSTRAINT %I', constraint_name);
    END LOOP;
END $$;
ALTER TABLE public.agent_enrollment_policies
    DROP COLUMN IF EXISTS access_token_ttl_seconds,
    ALTER COLUMN access_token_ttl SET DEFAULT interval '1 hour',
    ALTER COLUMN access_token_ttl SET NOT NULL,
    ADD CONSTRAINT agent_enrollment_policies_access_token_ttl_check
        CHECK (access_token_ttl >= interval '5 minutes' AND access_token_ttl <= interval '24 hours');
