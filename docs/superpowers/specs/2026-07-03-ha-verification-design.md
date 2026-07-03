# Design: High-Availability verification tests

**Date:** 2026-07-03
**Status:** Approved (pending implementation plan)

## Problem

unified-cd claims high availability (stateless controller replicas, PostgreSQL
advisory-lock leader election, `FOR UPDATE SKIP LOCKED` conflict-free run
claiming, graceful drain) in [docs/high-availability.md](../../high-availability.md).
Today only the **primitives** are tested: `TestAcquireAdvisoryLock_AcquireAndRelease`
(advisory-lock mutual exclusion) and `TestScheduler_OnlyOneLeaderQueues`. The
**system-level HA behavior** — real multi-replica failover, no double execution
under concurrency, no lost runs, API availability during a leader kill — is
unverified. This design adds tests that verify those end-to-end.

## Goal

Two complementary test levels, each mapping HA-doc claims to a concrete,
measurable check:

- **Level 1** — deterministic Go integration tests (real Postgres via the
  existing `NewTestPostgres` helper) that run in normal CI. Cheap, fast,
  regression-catching.
- **Level 2** — a Docker-Compose multi-replica failover harness driven by a
  `//go:build ha` Go test, proving controller failover end-to-end.

## Scope decisions (from brainstorming)

| Question | Decision |
|---|---|
| Level 2 harness form | **Go test driving `docker compose`** (`//go:build ha`), run via `go test -tags ha ./test/ha/`. Mirrors the repo's `//go:build k8s` integration-test convention. |
| Level 2 failure injection | Leader **graceful stop** (`docker stop` = SIGTERM), leader **crash** (`docker kill` = SIGKILL), and **non-leader kill** (no-impact control). |
| Agent in Level 2 | **Standard containerized agent** (Linux — avoids Windows host-shell issues). The controller-side claim/report/failover path it exercises is identical for the k8s-agent. |
| S3 (Garage) | **Omitted** from the Level 2 stack. The controller starts without S3 (log archival disabled); none of the HA invariants under test need it. |
| k8s-agent HA | **Deferred** to a separate follow-up (needs a real kind/minikube cluster; the controller-tier claims are already covered by the standard-agent Level 2). |
| DB failure / pooler faults / hard network partition timing | **Out of scope** here (that is "Level 3" chaos — a later effort). |

## Non-goals (YAGNI)

- Level 3 chaos (DB pause/partition, PgBouncer session-vs-transaction pooling,
  hard-failure TCP-keepalive lock-release timing).
- k8s-agent-under-failover (kind cluster) — separate follow-up.
- Load/throughput benchmarking — this verifies correctness, not performance.

## Level 1 — Go integration tests

Deterministic, Postgres-backed (`NewTestPostgres`), run under normal
`go test ./internal/...`. Each verifies one HA claim.

### 1. `TestHA_NoDoubleClaim` (`internal/store`)
Verifies: `FOR UPDATE SKIP LOCKED` claiming is conflict-free (no run claimed
twice by concurrent claimers — the core "no double execution" guarantee).

- Seed a job; create M (e.g. 50) runs and transition them to Queued.
- Launch N (e.g. 8) goroutines that each loop calling the store's claim/dequeue
  method (the same `SKIP LOCKED` path agents use) until the queue is empty,
  recording every claimed run id.
- Assert: total distinct claims == M, and **no run id claimed by more than one
  goroutine** (union of per-goroutine sets is disjoint; sum of counts == M).

### 2. `TestHA_AdvisoryLockReleasedOnConnClose` (`internal/store`)
Verifies: PostgreSQL auto-releases a session-level advisory lock when the
holder's connection dies (the crash-failover path — not graceful unlock).

- Open a dedicated `pgx` connection (not the pool) to the same test DB, acquire
  `pg_advisory_lock(testKey)` on it.
- Confirm a second acquire attempt (via the pool) fails (lock held).
- **Hard-close the dedicated connection** (`conn.Close`/kill the socket) to
  simulate a crashed replica (no graceful unlock).
- Assert: within a bounded poll (e.g. ≤ 5 s) another acquire of the same key
  succeeds — proving PG released the lock on session end.

### 3. `TestHA_SchedulerFailover` (`internal/controller`)
Verifies: after the scheduler leader is lost, a surviving replica takes over and
no pending runs are lost.

- Start two `RunScheduler` goroutines against the same store with a fast tick
  and independent cancelable contexts (two contending "replicas").
- Let one become leader (queue a pending run; confirm exactly one leader acted).
- **Cancel the leader's context** (simulate the leader going down → its advisory
  lock releases).
- Insert new Pending runs; assert the surviving scheduler acquires leadership
  and transitions them Pending→Queued within a bounded time (e.g. ≤ 5 s).

**Acceptance (Level 1):** all three pass deterministically in seconds under
`go test ./internal/store/ ./internal/controller/` with Postgres available.

## Level 2 — Docker-Compose failover harness

A `//go:build ha` Go test drives a real multi-replica stack and injects leader
failures, asserting the HA invariants end-to-end.

### Stack — `test/ha/docker-compose.ha.yaml`

| Service | Details |
|---|---|
| `postgres` ×1 | Shared DB; all replicas connect to it. |
| `controller` ×3 | Built from **`docker/controller.Dockerfile`** (production image — real process-kill semantics, not the `air` dev image). Identical env on all: `UNIFIED_DB_DSN`, `UNIFIED_TOKEN`, `UNIFIED_AGENT_TOKEN`, `UNIFIED_CONTROLLER_KEY`. No host ports (only reachable via nginx). |
| `nginx` ×1 | L7 load balancer, round-robin to the 3 controllers, `/healthz` health check, exposes a single host port. |
| `agent` ×2 | Containerized `cmd/agent` (a lean Dockerfile is added if none exists), same label pool, pointing at the nginx URL. Executes trivial job steps (short `sleep`/`echo`). |

S3/Garage omitted (log archival disabled — not needed).

### Driver — `test/ha/ha_test.go` (`//go:build ha`)

1. **Bring up** the stack: `docker compose -f test/ha/docker-compose.ha.yaml up -d --build`; wait until nginx `/readyz` is healthy and all 3 controllers registered.
2. **Identify the leader**: tail controller logs for `scheduler became leader`
   (the leader logs this on acquiring the advisory lock).
3. **Submit workload**: M jobs (short-sleep step) via the API through nginx.
4. **Failure matrix** (each step re-identifies the current leader):
   - (i) `docker kill` a **non-leader** → assert no disruption (control case).
   - (ii) `docker stop` the **leader** (SIGTERM / graceful drain path) → assert a new leader is elected.
   - (iii) `docker kill` the **leader** (SIGKILL / connection-close path) → assert a new leader is elected.
5. During injection, a background goroutine **continuously polls the API** and
   records any 5xx responses.
6. **Teardown**: `docker compose down -v` in a deferred cleanup (runs even on
   test failure).

### Invariants asserted (the "is HA real?" scorecard)

| Invariant | Check | Threshold |
|---|---|---|
| **No double execution** | Each run has exactly one terminal result / one set of step reports (query runs + step_reports; no run executed twice). | 0 double |
| **No lost runs** | Every submitted run reaches `Succeeded`. | 0 lost |
| **Failover time** | After a leader kill, a newly-submitted run transitions Pending→Queued within T. | ≤ 10 s (graceful and crash) |
| **API availability** | 5xx responses during the kill window. | Transient at the kill instant tolerated; no sustained 5xx (define a small bound, e.g. ≤ 2 consecutive). |

### Runnability

- Tagged `//go:build ha`, excluded from normal `go test`. Run via
  `go test -tags ha ./test/ha/` (requires Docker).
- Add a `make ha-test` target.
- The test skips (with a clear message) if Docker/compose is unavailable.

## Touch points summary

| Path | Purpose |
|---|---|
| `internal/store/postgres_ha_test.go` | Level 1 tests 1 & 2 |
| `internal/controller/scheduler_ha_test.go` | Level 1 test 3 |
| `test/ha/docker-compose.ha.yaml` | Level 2 stack |
| `test/ha/nginx.conf` | LB config |
| `docker/agent.Dockerfile` (if none exists) | Containerized standard agent |
| `test/ha/ha_test.go` | `//go:build ha` driver + assertions |
| `Makefile` | `ha-test` target |
| `docs/high-availability.md` | (optional) link to the verification tests |

## Acceptance criteria

- Level 1: three deterministic tests pass in CI (Postgres-backed), each mapped
  to a stated HA claim.
- Level 2: `go test -tags ha ./test/ha/` brings up 3 controllers + nginx + 2
  agents, injects the three failure types, and asserts **0 double executions,
  0 lost runs, failover ≤ 10 s, no sustained 5xx** — then tears the stack down
  cleanly.
