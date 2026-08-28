-- run_pod_bindings records which Kubernetes Pod (by name and UID) a
-- Kubernetes agent has most recently reported as executing a run, so the
-- store-credentials broker (internal/controller/api_store_credentials.go)
-- can refuse a Pod asking for a run it is not executing. See
-- docs/superpowers/specs/2026-08-26-sidecar-credential-delivery-design.md
-- §5.6.
--
-- One row per run, upserted from the k8s agent's periodic heartbeat
-- (HeartbeatRequest.PodBindings) — a run reclaimed after an agent restart
-- or a stuck-run reconcile simply overwrites the previous Pod's row, there
-- is no history to keep. It does not need to outlive the run: broker
-- requests only happen while a run is actively executing, so ON DELETE
-- CASCADE (mirroring sidecar_status, migration 010) is enough cleanup —
-- there is no separate deletion on run completion.
--
-- pod_uid, not pod_name, is what the broker compares: a Pod name can be
-- reused (a new Pod created under the same generated name after the old
-- one is deleted), a Pod UID never is. pod_name is kept purely for
-- operator-facing logging — see api.PodBinding's doc comment.
CREATE TABLE public.run_pod_bindings (
    run_id     uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    pod_name   text NOT NULL,
    pod_uid    text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id)
);
