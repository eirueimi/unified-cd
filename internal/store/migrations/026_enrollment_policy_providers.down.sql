DELETE FROM public.agent_enrollment_policies WHERE provider <> 'kubernetes';

ALTER TABLE public.agent_enrollment_policies
    DROP CONSTRAINT IF EXISTS agent_enrollment_policies_provider_check;
ALTER TABLE public.agent_enrollment_policies
    ADD CONSTRAINT agent_enrollment_policies_provider_check CHECK (provider='kubernetes');
