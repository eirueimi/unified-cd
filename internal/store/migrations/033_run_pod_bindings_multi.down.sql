-- Keep only the most recently reported Pod of each run, as before.
DELETE FROM public.run_pod_bindings b
 USING public.run_pod_bindings n
 WHERE b.run_id = n.run_id
   AND (b.updated_at, b.pod_uid) < (n.updated_at, n.pod_uid);
DROP INDEX public.run_pod_bindings_run_pod_uid_key;
ALTER TABLE public.run_pod_bindings ADD CONSTRAINT run_pod_bindings_pkey PRIMARY KEY (run_id);
