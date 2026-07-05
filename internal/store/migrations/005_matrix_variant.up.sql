ALTER TABLE public.step_reports ADD COLUMN variant text NOT NULL DEFAULT '';
ALTER TABLE public.step_reports DROP CONSTRAINT step_reports_pkey;
ALTER TABLE public.step_reports ADD CONSTRAINT step_reports_pkey PRIMARY KEY (run_id, step_index, variant);

ALTER TABLE public.step_outputs ADD COLUMN variant text NOT NULL DEFAULT '';
ALTER TABLE public.step_outputs DROP CONSTRAINT step_outputs_pkey;
ALTER TABLE public.step_outputs ADD CONSTRAINT step_outputs_pkey PRIMARY KEY (run_id, step_index, variant, key);
