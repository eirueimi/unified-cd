-- Consolidated initial schema for unified-cd.
-- Squashed from the former incremental migrations 001-017 via pg_dump of the
-- fully-migrated schema. Breaking change: databases created by the old numbered
-- migrations are NOT upgraded by this file; it defines the schema for fresh installs.

--
--






--
-- Name: agents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.agents (
    id text NOT NULL,
    hostname text NOT NULL,
    os text NOT NULL,
    labels text[] DEFAULT '{}'::text[] NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    version text DEFAULT ''::text NOT NULL,
    env jsonb DEFAULT '{}'::jsonb NOT NULL
);


--
-- Name: app_sources; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_sources (
    name text NOT NULL,
    spec jsonb NOT NULL,
    last_synced_at timestamp with time zone,
    last_commit text DEFAULT ''::text NOT NULL,
    managed_jobs text[] DEFAULT '{}'::text[] NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: controller_settings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.controller_settings (
    id integer DEFAULT 1 NOT NULL,
    controller_key_hex text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT controller_settings_id_check CHECK ((id = 1))
);


--
-- Name: git_credentials; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.git_credentials (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    host text NOT NULL,
    cred_type text NOT NULL,
    secret_ref text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT git_credentials_cred_type_check CHECK ((cred_type = ANY (ARRAY['token'::text, 'sshKey'::text])))
);


--
-- Name: jobs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.jobs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    api_version text NOT NULL,
    spec jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: logs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.logs (
    run_id uuid NOT NULL,
    seq bigint NOT NULL,
    step_index integer NOT NULL,
    stream text NOT NULL,
    ts timestamp with time zone NOT NULL,
    line text NOT NULL,
    CONSTRAINT logs_stream_check CHECK ((stream = ANY (ARRAY['stdout'::text, 'stderr'::text])))
);


--
-- Name: logs_seq_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.logs_seq_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: logs_seq_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.logs_seq_seq OWNED BY public.logs.seq;


--
-- Name: mutex_holders; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.mutex_holders (
    mutex_name text NOT NULL,
    run_id uuid NOT NULL,
    acquired_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: named_lock_slots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.named_lock_slots (
    pool_name text NOT NULL,
    slot_id integer NOT NULL,
    run_id uuid,
    acquired_at timestamp with time zone
);


--
-- Name: oidc_states; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.oidc_states (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    state text NOT NULL,
    redirect_to text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL
);


--
-- Name: pats; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.pats (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    token_hash text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone,
    last_used_at timestamp with time zone
);


--
-- Name: run_approvals; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.run_approvals (
    run_id uuid NOT NULL,
    step_index integer NOT NULL,
    step_name text NOT NULL,
    message text DEFAULT ''::text NOT NULL,
    status text NOT NULL,
    decided_by text,
    comment text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    timeout_at timestamp with time zone,
    decided_at timestamp with time zone
);


--
-- Name: run_log_archives; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.run_log_archives (
    run_id uuid NOT NULL,
    object_key text NOT NULL,
    size_bytes bigint DEFAULT 0 NOT NULL,
    archived_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: run_outputs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.run_outputs (
    run_id uuid NOT NULL,
    key text NOT NULL,
    value text NOT NULL
);


--
-- Name: runs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.runs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    job_name text NOT NULL,
    status text DEFAULT 'Pending'::text NOT NULL,
    params jsonb DEFAULT '{}'::jsonb NOT NULL,
    spec jsonb NOT NULL,
    claimed_by text,
    claimed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    agent_selector text[] DEFAULT '{}'::text[] NOT NULL,
    triggered_by text DEFAULT 'api'::text NOT NULL,
    CONSTRAINT runs_status_check CHECK ((status = ANY (ARRAY['Pending'::text, 'Queued'::text, 'Running'::text, 'Succeeded'::text, 'Failed'::text, 'Cancelled'::text])))
);


--
-- Name: schedules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.schedules (
    name text NOT NULL,
    cron text NOT NULL,
    job_name text NOT NULL,
    params jsonb DEFAULT '{}'::jsonb NOT NULL,
    last_fired_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: secrets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.secrets (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    scope text DEFAULT 'global'::text NOT NULL,
    scope_ref text DEFAULT ''::text NOT NULL,
    encrypted_dek bytea NOT NULL,
    ciphertext bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.sessions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    token_hash text NOT NULL,
    sub text NOT NULL,
    email text NOT NULL,
    refresh_token text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    last_used_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: step_outputs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.step_outputs (
    run_id uuid NOT NULL,
    step_index integer NOT NULL,
    key text NOT NULL,
    value text NOT NULL
);


--
-- Name: step_reports; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.step_reports (
    run_id uuid NOT NULL,
    step_index integer NOT NULL,
    status text NOT NULL,
    exit_code integer,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    step_name text DEFAULT ''::text NOT NULL,
    stage_index integer DEFAULT 0 NOT NULL,
    CONSTRAINT step_reports_status_check CHECK ((status = ANY (ARRAY['Running'::text, 'Succeeded'::text, 'Failed'::text, 'Cancelled'::text, 'Skipped'::text, 'WaitingApproval'::text])))
);


--
-- Name: webhook_receivers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.webhook_receivers (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    spec jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: logs seq; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.logs ALTER COLUMN seq SET DEFAULT nextval('public.logs_seq_seq'::regclass);


--
-- Name: agents agents_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agents
    ADD CONSTRAINT agents_pkey PRIMARY KEY (id);


--
-- Name: app_sources app_sources_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_sources
    ADD CONSTRAINT app_sources_pkey PRIMARY KEY (name);


--
-- Name: controller_settings controller_settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.controller_settings
    ADD CONSTRAINT controller_settings_pkey PRIMARY KEY (id);


--
-- Name: git_credentials git_credentials_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_credentials
    ADD CONSTRAINT git_credentials_name_key UNIQUE (name);


--
-- Name: git_credentials git_credentials_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_credentials
    ADD CONSTRAINT git_credentials_pkey PRIMARY KEY (id);


--
-- Name: jobs jobs_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_name_key UNIQUE (name);


--
-- Name: jobs jobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_pkey PRIMARY KEY (id);


--
-- Name: logs logs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.logs
    ADD CONSTRAINT logs_pkey PRIMARY KEY (run_id, seq);


--
-- Name: mutex_holders mutex_holders_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mutex_holders
    ADD CONSTRAINT mutex_holders_pkey PRIMARY KEY (mutex_name);


--
-- Name: named_lock_slots named_lock_slots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.named_lock_slots
    ADD CONSTRAINT named_lock_slots_pkey PRIMARY KEY (pool_name, slot_id);


--
-- Name: oidc_states oidc_states_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oidc_states
    ADD CONSTRAINT oidc_states_pkey PRIMARY KEY (id);


--
-- Name: oidc_states oidc_states_state_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.oidc_states
    ADD CONSTRAINT oidc_states_state_key UNIQUE (state);


--
-- Name: pats pats_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pats
    ADD CONSTRAINT pats_pkey PRIMARY KEY (id);


--
-- Name: pats pats_token_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pats
    ADD CONSTRAINT pats_token_hash_key UNIQUE (token_hash);


--
-- Name: run_approvals run_approvals_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_approvals
    ADD CONSTRAINT run_approvals_pkey PRIMARY KEY (run_id, step_index);


--
-- Name: run_log_archives run_log_archives_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_log_archives
    ADD CONSTRAINT run_log_archives_pkey PRIMARY KEY (run_id);


--
-- Name: run_outputs run_outputs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_outputs
    ADD CONSTRAINT run_outputs_pkey PRIMARY KEY (run_id, key);


--
-- Name: runs runs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_pkey PRIMARY KEY (id);


--
-- Name: schedules schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.schedules
    ADD CONSTRAINT schedules_pkey PRIMARY KEY (name);


--
-- Name: secrets secrets_name_scope_scope_ref_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secrets
    ADD CONSTRAINT secrets_name_scope_scope_ref_key UNIQUE (name, scope, scope_ref);


--
-- Name: secrets secrets_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secrets
    ADD CONSTRAINT secrets_pkey PRIMARY KEY (id);


--
-- Name: sessions sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (id);


--
-- Name: sessions sessions_token_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_token_hash_key UNIQUE (token_hash);


--
-- Name: step_outputs step_outputs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.step_outputs
    ADD CONSTRAINT step_outputs_pkey PRIMARY KEY (run_id, step_index, key);


--
-- Name: step_reports step_reports_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.step_reports
    ADD CONSTRAINT step_reports_pkey PRIMARY KEY (run_id, step_index);


--
-- Name: webhook_receivers webhook_receivers_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.webhook_receivers
    ADD CONSTRAINT webhook_receivers_name_key UNIQUE (name);


--
-- Name: webhook_receivers webhook_receivers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.webhook_receivers
    ADD CONSTRAINT webhook_receivers_pkey PRIMARY KEY (id);


--
-- Name: idx_git_credentials_host; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_git_credentials_host ON public.git_credentials USING btree (host);


--
-- Name: idx_schedules_job_name; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_schedules_job_name ON public.schedules USING btree (job_name);


--
-- Name: logs_run_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX logs_run_idx ON public.logs USING btree (run_id, seq);


--
-- Name: runs_status_created_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX runs_status_created_idx ON public.runs USING btree (status, created_at);


--
-- Name: logs logs_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.logs
    ADD CONSTRAINT logs_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.runs(id) ON DELETE CASCADE;


--
-- Name: mutex_holders mutex_holders_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mutex_holders
    ADD CONSTRAINT mutex_holders_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.runs(id) ON DELETE CASCADE;


--
-- Name: named_lock_slots named_lock_slots_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.named_lock_slots
    ADD CONSTRAINT named_lock_slots_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.runs(id) ON DELETE SET NULL;


--
-- Name: run_approvals run_approvals_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_approvals
    ADD CONSTRAINT run_approvals_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.runs(id) ON DELETE CASCADE;


--
-- Name: run_log_archives run_log_archives_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_log_archives
    ADD CONSTRAINT run_log_archives_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.runs(id) ON DELETE CASCADE;


--
-- Name: run_outputs run_outputs_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.run_outputs
    ADD CONSTRAINT run_outputs_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.runs(id) ON DELETE CASCADE;


--
-- Name: runs runs_job_name_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runs
    ADD CONSTRAINT runs_job_name_fkey FOREIGN KEY (job_name) REFERENCES public.jobs(name) ON DELETE CASCADE;


--
-- Name: schedules schedules_job_name_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.schedules
    ADD CONSTRAINT schedules_job_name_fkey FOREIGN KEY (job_name) REFERENCES public.jobs(name) ON DELETE CASCADE;


--
-- Name: step_outputs step_outputs_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.step_outputs
    ADD CONSTRAINT step_outputs_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.runs(id) ON DELETE CASCADE;


--
-- Name: step_reports step_reports_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.step_reports
    ADD CONSTRAINT step_reports_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.runs(id) ON DELETE CASCADE;


--
--
