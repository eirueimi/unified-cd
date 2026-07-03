# Design: HA hard-failure lock-release verification

**Date:** 2026-07-03
**Status:** Approved (pending implementation plan)

## Problem

The HA verification suite (merged) proves controller failover for graceful stop
and process crash (connection-close) — both release the advisory lock in
sub-second time. It does NOT cover the **hard-failure** path that
[docs/high-availability.md](../../high-availability.md) itself flags as the
weakest link: a network partition with no FIN/RST, where PostgreSQL can only
detect the dead lock-holder via TCP keepalives. The doc says this window is
"up to several minutes" unless `tcp_keepalives_*` are tuned, but nothing
verifies that tuning actually bounds it. This design adds that test.

## Goal

Prove that, with tuned PostgreSQL TCP keepalives, a hard network partition of
the scheduler leader releases its advisory lock and a follower takes over
within a bounded time (target ≤ 20 s), rather than hanging for PostgreSQL's
default (~2 h). Fail the test if the lock does not release in bound (which
would be an effective split-brain / stuck-scheduler).

## Key approach decisions (from brainstorming)

| Question | Decision |
|---|---|
| How to simulate "no-FIN" partition | **`docker network disconnect`** the leader container from the PG network. Removing the interface blackholes existing TCP connections (no FIN/RST), so PG must use keepalives to detect death. (toxiproxy application toxics are rejected — the underlying TCP socket stays alive and answers keepalives, so they do not simulate a TCP-level partition.) |
| PG keepalive tuning | `tcp_keepalives_idle=5`, `tcp_keepalives_interval=2`, `tcp_keepalives_count=2` → detection ≈ 5 + 2×2 = ~9 s. |
| Stack | Minimal: `postgres` (tuned) + `controller1` + `controller2`, both running `RunScheduler`. No nginx/agents (the advisory-lock + scheduler-leader layer is what's under test). |
| Failover observation | The surviving controller logs `scheduler became leader` AND queues a newly-created Pending run. |
| Bound / threshold | ≤ 20 s (keepalive detection ~9 s + scheduler tick + margin). Tunable after a few real measurements. |
| The "untuned = ~2 h" negative | NOT tested (impractical to wait). Documented instead. |

## Non-goals (YAGNI)

- The untuned/default-timeout case (would take ~2 h to observe).
- PgBouncer, PG primary→standby failover, SSE cross-replica, k8s-agent — separate
  deferred items.
- Nginx/agents — not needed for the advisory-lock/leader property under test.

## Design

### Stack — `test/ha/docker-compose.hardfail.yaml`

| Service | Details |
|---|---|
| `postgres` ×1 | `postgres:16-alpine`, command adds `-c tcp_keepalives_idle=5 -c tcp_keepalives_interval=2 -c tcp_keepalives_count=2`. On a dedicated user-defined network (so the leader can be disconnected from it). Healthcheck. |
| `controller1`, `controller2` | Built from `docker/controller.Dockerfile`, identical env (`UNIFIED_DB_DSN`, `UNIFIED_TOKEN`, `UNIFIED_CONTROLLER_KEY`). No host ports needed (the test reads logs + submits runs via `docker exec`/a temporarily-exposed port, or via the DB — see driver). Both run `RunScheduler`; exactly one holds the advisory lock. |

To let the test submit a run and observe status, expose ONE controller's port
(e.g. `controller2` on `18081`) OR have the test query the DB directly through a
short-lived connection. Preferred: expose both controllers on distinct host
ports so the test can talk to a specific replica (needed anyway to submit via
the surviving replica after the leader is partitioned).

### Driver — `test/ha/hardfailure_test.go` (`//go:build ha`)

1. Bring up the stack (`docker compose -f docker-compose.hardfail.yaml up -d --build`); wait until both controllers are ready.
2. Identify the scheduler leader via `docker compose logs` (`scheduler became leader`).
3. Create a Pending run and confirm the leader queues it (baseline: leader is alive).
4. **Partition**: `docker network disconnect <ha-hardfail-network> <leader-container>`; record `t0 = now`.
5. Create a NEW Pending run (via the surviving controller's port), then poll until it reaches `Queued`. Also grep the surviving controller's logs for `scheduler became leader`.
6. Measure `elapsed = now - t0`. **Assert** the surviving controller took over (new run Queued + leader log) within the bound (≤ 20 s). Fail with the measured time otherwise.
7. Teardown (`docker compose down -v`) via `t.Cleanup` even on failure. Docker-unavailable → SKIP.

### Falsifier

If PG did not release the lock within the bound (keepalives ineffective/not
tuned, or a real stuck-leader bug), no follower can acquire it → the new run
stays Pending → the poll times out → `t.Fatal` with the measured elapsed time.
The test cannot pass unless the hard-partition lock release actually happened
within bound.

### Docs

Update `docs/high-availability.md` §"Hard failure mitigation": note that the
tuned-keepalive bound is now verified by `test/ha/hardfailure_test.go`, and that
WITHOUT the `tcp_keepalives_*` tuning the release falls back to PostgreSQL's
default (~2 h) — so the tuning is required for bounded hard-failure failover.

## Touch points

| Path | Purpose |
|---|---|
| `test/ha/docker-compose.hardfail.yaml` | tuned-PG + 2-controller stack |
| `test/ha/hardfailure_test.go` | `//go:build ha` partition driver + assertion |
| `Makefile` | fold into the `ha-test` target (already runs `test/ha/...` with `-tags ha`) |
| `docs/high-availability.md` | note the verified bound + keepalive requirement |

## Acceptance criteria

- `go test -tags ha ./test/ha/` (or `make ha-test`) runs the hard-failure test:
  the scheduler leader is hard-partitioned via `docker network disconnect`, and a
  follower takes over within ≤ 20 s (measured), with the test failing if it does
  not. Stack is torn down cleanly. The `docs/high-availability.md` hard-failure
  note reflects the verified behavior.
