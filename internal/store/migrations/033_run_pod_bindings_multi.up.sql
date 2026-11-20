-- A run may be bound to several Kubernetes Pods: its claim Pod plus every
-- isolated scope Pod the same agent creates for it (each runs its own artifact
-- sidecar, which authenticates with its own Pod's projected token). The
-- primary key moves from (run_id) to (run_id, pod_uid). Existing rows, one per
-- run, stay valid unchanged.
ALTER TABLE public.run_pod_bindings DROP CONSTRAINT run_pod_bindings_pkey;
CREATE UNIQUE INDEX run_pod_bindings_run_pod_uid_key ON public.run_pod_bindings (run_id, pod_uid);
