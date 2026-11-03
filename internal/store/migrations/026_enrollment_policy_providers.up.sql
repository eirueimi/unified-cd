-- Allow cloud providers in enrollment policies. Migration 013 created the
-- table with CHECK (provider='kubernetes'); find that constraint by its
-- definition rather than assuming its generated name.
DO $$
DECLARE constraint_name text;
BEGIN
    FOR constraint_name IN
        SELECT conname FROM pg_constraint
         WHERE conrelid = 'public.agent_enrollment_policies'::regclass
           AND contype = 'c'
           AND pg_get_constraintdef(oid) ~ '\mprovider\M'
    LOOP
        EXECUTE format('ALTER TABLE public.agent_enrollment_policies DROP CONSTRAINT %I', constraint_name);
    END LOOP;
END $$;

ALTER TABLE public.agent_enrollment_policies
    ADD CONSTRAINT agent_enrollment_policies_provider_check
        CHECK (provider IN ('kubernetes','aws','gcp','azure'));
