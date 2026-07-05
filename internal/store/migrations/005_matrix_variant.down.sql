DELETE FROM public.step_reports WHERE variant <> '';
ALTER TABLE public.step_reports DROP CONSTRAINT step_reports_pkey;
ALTER TABLE public.step_reports ADD CONSTRAINT step_reports_pkey PRIMARY KEY (run_id, step_index);
ALTER TABLE public.step_reports DROP COLUMN variant;

DELETE FROM public.step_outputs WHERE variant <> '';
ALTER TABLE public.step_outputs DROP CONSTRAINT step_outputs_pkey;
ALTER TABLE public.step_outputs ADD CONSTRAINT step_outputs_pkey PRIMARY KEY (run_id, step_index, key);
ALTER TABLE public.step_outputs DROP COLUMN variant;
