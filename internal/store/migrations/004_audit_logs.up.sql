CREATE TABLE public.audit_logs (
    id bigserial NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    actor text NOT NULL,
    method text NOT NULL,
    path text NOT NULL,
    action text NOT NULL,
    resource text DEFAULT ''::text NOT NULL,
    status integer NOT NULL,
    CONSTRAINT audit_logs_pkey PRIMARY KEY (id)
);

CREATE INDEX idx_audit_logs_occurred_at ON public.audit_logs USING btree (occurred_at);
