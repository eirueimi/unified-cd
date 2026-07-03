# High Availability (HA) Guide

This document covers how to run unified-cd controllers in a redundant, single-point-of-failure-free configuration.

## Table of Contents

- [Design Principles](#design-principles)
- [HA Architecture](#ha-architecture)
- [Per-Component HA Behavior](#per-component-ha-behavior)
- [Leader Failover Sequence](#leader-failover-sequence)
- [Required Configuration](#required-configuration)
- [Load Balancer and Sessions](#load-balancer-and-sessions)
- [Rolling Deploys and Graceful Shutdown](#rolling-deploys-and-graceful-shutdown)
- [External Dependency Redundancy](#external-dependency-redundancy)
- [Agent Redundancy](#agent-redundancy)
- [Deployment Examples](#deployment-examples)
- [Failure Scenarios and Behavior](#failure-scenarios-and-behavior)
- [HA Checklist](#ha-checklist)

---

## Design Principles

The unified-cd controller is designed to be **stateless**.
All persistent state lives externally:

- **PostgreSQL** — single source of truth for jobs, runs, queues, schedules, PATs, sessions, and encrypted secrets.
- **S3-compatible object store (Garage/S3)** — log archives, artifacts, and git template caches.

Because the controller holds no in-memory persistent state, **running N instances behind a load balancer**
is all that is needed for horizontal scaling and redundancy.
Coordination between replicas happens entirely via PostgreSQL — no in-memory inter-replica communication.

---

## HA Architecture

```
                    ┌──────────────┐
   Browser / CLI ──►│ Load Balancer │  (L7, /healthz for health checks)
   Agents           └──────┬───────┘
                           │  distributed (no sticky sessions needed)
        ┌──────────────────┼──────────────────┐
        ▼                  ▼                  ▼
 ┌────────────┐     ┌────────────┐     ┌────────────┐
 │controller 1│     │controller 2│     │controller 3│   ← stateless, N instances
 └─────┬──────┘     └─────┬──────┘     └─────┬──────┘
       └──────────────────┼──────────────────┘
              ┌───────────┴───────────┐
              ▼                       ▼
      ┌───────────────┐      ┌───────────────────┐
      │ PostgreSQL(HA) │      │ S3 / Garage (HA)  │
      │ primary+standby│      │  logs / artifacts │
      └───────────────┘      └───────────────────┘
```

- All replicas share the same PostgreSQL and S3.
- Work that must run on exactly one instance (e.g. the scheduler) is arbitrated via
  PostgreSQL **advisory locks** for leader election.
- Claiming individual Runs is distributed conflict-free via `FOR UPDATE SKIP LOCKED`.

---

## Per-Component HA Behavior

### API server (request processing)

Completely stateless. Any replica reads and writes PostgreSQL/S3 for every request,
so the LB can freely distribute traffic (no sticky sessions required — see below).

### Background jobs

Each replica starts multiple background goroutines at startup.
Jobs that must not run on more than one replica are restricted to a single leader
via PostgreSQL **session-level advisory locks**.
When the leader goes down, the lock is released and another replica picks it up on the next tick.

| Job | Arbitration | Behavior with multiple replicas |
|-----|------------|----------------------------------|
| Scheduler (`RunScheduler`) | advisory lock (`schedulerLockKey`) | Only the leader transitions Pending→Queued and fires schedules |
| Log archiver (`RunLogArchiver`) | advisory lock (`logArchiverLockKey`) | Only the leader archives logs |
| Cache cleanup (`RunCacheCleanup`) | advisory lock (`cacheCleanupLockKey`) | Only the leader deletes expired cache entries |
| AppSource reconciler (`RunAppSourceReconciler`) | advisory lock (`appSourceReconcilerLockKey`) | Only the leader reconciles |
| Git resolver (`RunGitResolver`) | none (idempotent) | Runs on all replicas. `git://` URI resolution is idempotent and harmless if duplicated |
| OIDC state cleanup | none (idempotent) | Runs on all replicas. Expired state deletion is idempotent |

> Advisory locks are held at session level on a dedicated DB connection for the lifetime of the goroutine.
> They are released on ctx cancellation or error. If the leader crashes (process stop = connection close),
> PostgreSQL automatically releases the lock, so failover is automatic.

---

## Leader Failover Sequence

Leadership is represented by a single PostgreSQL **session-level advisory lock**
(key `0x65786364` = 'excd'). The holder of this lock is the leader.
No external coordinator is needed.

### Normal operation (200ms tick)

```
Each replica's RunScheduler goroutine:

  tick ──► Am I the leader? (release != nil)
            │
            ├─ YES → run scheduling logic
            │
            └─ NO  → try pg_try_advisory_lock('excd') (non-blocking)
                       │
                       ├─ acquired → became leader, log "scheduler became leader"
                       └─ failed   → another replica is leader, skip until next tick
```

`pg_try_advisory_lock` is non-blocking (acquires immediately if free, returns `false` if held).
The lock is held at session level on a dedicated DB connection **for as long as that connection lives**.

### Failover sequence after leader loss

**Step 1: lock release** (speed varies by failure type)

| Failure type | How the lock is released | Release speed |
|---|---|---|
| Graceful stop (SIGTERM / rolling deploy) | ctx cancel → `defer release()` calls `pg_advisory_unlock` | Immediate |
| Process crash (panic / OOM / kill -9) | OS closes the socket → PG detects session end and auto-releases | Usually within a few seconds |
| Hard failure (power loss / network partition, no FIN) | PG detects connection death via TCP keepalive | **Up to several minutes** (tuning required) |

Because advisory locks are session-level, PostgreSQL auto-releases when it detects the connection is gone,
even if the app never explicitly unlocks.

**Step 2: another replica becomes leader**

Once the lock is free, the **first surviving follower to reach its next tick** wins
`pg_try_advisory_lock` and becomes leader.
Even if multiple replicas try simultaneously, PostgreSQL only grants the lock to one,
so **split-brain cannot occur**.

**Estimated failover time**

```
Graceful / crash:
  lock release (immediate to a few seconds) + next tick (≤200ms) = well under a second to a few seconds

Hard failure:
  PG keepalive detection (minutes by default) + next tick (≤200ms)
  → tune tcp_keepalives_idle / tcp_keepalives_interval to reduce this
```

### What happens during leader absence

Only **Pending→Queued transitions and schedule fires are paused** while there is no leader.

- Already Queued / in-progress Runs continue — agents keep executing, and claiming uses `SKIP LOCKED` across all replicas.
- API requests are handled by all replicas without interruption.
- After promotion, the new leader processes any accumulated Pending Runs on the next tick — no runs are lost.

### Hard failure mitigation

To reduce the window for network-partition failures where FIN packets are not delivered,
tune PostgreSQL keepalive settings:

```
# postgresql.conf
tcp_keepalives_idle     = 10   # seconds before sending keepalives after last data
tcp_keepalives_interval = 5    # keepalive retransmit interval
tcp_keepalives_count    = 3    # retries before declaring the connection dead
# → worst case: 10 + 5×3 = 25 seconds until connection close → lock released
```

This tuned-keepalive bound is verified by `test/ha/hardfailure_test.go`: a `docker network disconnect` partition of the leader (a real no-FIN blackhole) causes PostgreSQL to release the advisory lock via TCP keepalive detection, and the survivor becomes leader and queues a new run within ~8 s (measured; `hfBound = 20 s`). **Without `tcp_keepalives_*` tuning, PostgreSQL falls back to OS defaults (often ~2 h), making hard-failure failover unbounded — the tuning is REQUIRED for bounded hard-failure failover.**

---

### Run distribution

Agents long-poll the controller to `claim` a Run.
Queue dequeue uses `FOR UPDATE SKIP LOCKED`, so **multiple agents and multiple controllers
claiming concurrently cannot cause double execution**.
Concurrency slot (`concurrency`) acquisition uses the same mechanism and is also conflict-free.

### Event delivery (SSE)

The log stream at `GET /api/v1/runs/{id}/events` is implemented via PostgreSQL
**LISTEN/NOTIFY** (`log_appended:{runID}` channel).
Because NOTIFY propagates even when the writing replica differs from the SSE-serving replica,
**SSE clients can connect to any replica** (no sticky sessions needed).

---

## Required Configuration

Settings that must match / be shared across all replicas for correct HA operation.

### ⚠️ `UNIFIED_CONTROLLER_KEY` (most critical)

The master key used for secret encryption. **Must be identical on all replicas.**

- If unset, each replica reads `controller_settings` in the shared DB. If no key exists yet,
  it generates one and saves it (safe even if multiple replicas start simultaneously — the DB
  serializes the write). Startup logs will show one of:
  - `controllerKey not set — generated a new key and persisted it to the database` (first generation)
  - `controllerKey not set — loaded previously persisted key from the database` (subsequent restarts)
- As long as all replicas share the same DB, omitting `UNIFIED_CONTROLLER_KEY` will not cause
  decryption failures. However, for production it is recommended to distribute the key explicitly
  via a Secret Manager or KMS for easier key rotation and backup management.
- If replicas point to **different DBs**, or if explicit `UNIFIED_CONTROLLER_KEY` values differ between
  replicas, a secret encrypted by one replica cannot be decrypted by another.

```bash
# Generate once (32 bytes hex) and share across all replicas
openssl rand -hex 32
```

### Other environment variables to keep consistent

| Variable | HA requirement |
|----------|----------------|
| `UNIFIED_CONTROLLER_KEY` | **Same on all replicas** (see above) |
| `UNIFIED_DB_DSN` | Must point to the HA PostgreSQL endpoint (primary or pooler) |
| `UNIFIED_TOKEN` | Same on all replicas (admin static token). Same applies to agent `UNIFIED_AGENT_TOKEN` |
| `UNIFIED_S3_*` | All replicas must point to the same S3/Garage |
| `UNIFIED_OIDC_*` | Same on all replicas when SSO is used (see [Authentication Guide](authentication.md)) |

> Auth state (PATs, sessions) is stored in PostgreSQL and can be validated by any replica.
> OIDC id_tokens are validated independently by each replica using JWT verification — no shared state needed.

---

## Load Balancer and Sessions

**Sticky sessions (session affinity) are not needed.**
As described above, auth state lives in the DB or JWT, and SSE is propagated to all replicas via NOTIFY.

- Use `/healthz` (see below) as the LB health check target.
- SSE is a long-lived connection; set the LB idle timeout generously (e.g. several minutes or more)
  and ensure buffering is disabled (the controller sends `X-Accel-Buffering: no`).

### Upstream failover tuning

unified-cd's own failover is sub-second (a new leader is elected within one scheduler tick
of the previous leader releasing its advisory lock). Delivering that sub-second failover to
clients is the **operator's LB responsibility**: with the LB's defaults, a hard-killed
(SIGKILL) controller leaves clients hanging on the now-dead TCP peer for the default connect
timeout (~60s on nginx), which undermines the fast failover the controllers provide.

Configure the LB to fail fast and re-try a healthy upstream. For nginx, the reference settings
(see `test/ha/nginx.conf` for a complete working example) are:

- `proxy_connect_timeout 2s` — a dead upstream fails the connect quickly instead of hanging
  for the ~60s default.
- `server controller:8080 max_fails=1 fail_timeout=5s` (per upstream) — eject a controller
  from rotation after a single failure so subsequent requests skip it.
- `proxy_next_upstream error timeout http_502 http_503 http_504` with
  `proxy_next_upstream_tries 3` and `proxy_next_upstream_timeout 8s` — retry the request
  against the next controller on a connect/response error, bounded so one request can traverse
  all three replicas within a few seconds.

The equivalent knobs on other load balancers (HAProxy `connect`/`retries`/`observe`, cloud L7
LBs' health-check and connection-drain settings) achieve the same goal: eject dead backends
fast and re-dispatch in-flight requests to a healthy replica.

---

## Rolling Deploys and Graceful Shutdown

On SIGINT/SIGTERM, the controller shuts down in stages:

1. Calls `SetShuttingDown()`, causing `/healthz` to return **503** → the LB stops sending new traffic (drain begins).
2. Waits ~2 seconds, then gracefully shuts down the HTTP server with up to a 10-second grace period.
3. Agent claim long-polls are immediately drained via `claimDrainCh`.

This enables one-at-a-time rolling deploys with no downtime.

### Health check endpoints

| Endpoint | Purpose | During shutdown |
|----------|---------|-----------------|
| `/healthz` | Liveness / LB health check | Returns 503 (for drain) |
| `/readyz` | Readiness (also checks DB connectivity) | Returns 503 |

In Kubernetes, assign `/healthz` as the liveness probe and `/readyz` as the readiness probe.
`/readyz` pings the DB, so replicas with a DB connectivity issue are automatically rotated out.

---

## External Dependency Redundancy

Redundant controllers are not enough if PostgreSQL or S3 is a single point of failure.

### PostgreSQL

- Use a managed HA service (Cloud SQL, Amazon RDS/Aurora, etc.) or a Patroni primary+standby setup.
- A connection pooler (PgBouncer etc.) is recommended to keep connection counts manageable across many replicas.
  - **Important**: advisory locks and LISTEN/NOTIFY are **session-level** features. If using a pooler,
    configure it in **session pooling mode** (transaction pooling breaks advisory locks and NOTIFY).
- After a PostgreSQL failover, controllers reconnect automatically and leader election re-runs.

### S3 / Object store

- For production, use managed S3 (high durability and availability). For Garage, use distributed mode
  (multiple nodes, `replication_factor` ≥ 2).
- The controller starts without S3, but log archival is disabled (`no object store configured — log archival disabled`).
  S3 is required for HA operation.

---

## Agent Redundancy

- Run **multiple agents** with the same label set (pool) — if one goes down, the others keep claiming jobs.
- Claiming uses `SKIP LOCKED`, so adding more agents increases throughput linearly.
- Agents also support graceful drain (stop claiming = cordon → finish in-progress Runs → exit).
- Agents can connect to any controller replica (behind the LB URL).

---

## Deployment Examples

### docker compose (simple scale-out)

```bash
# Scale to 3 controller replicas (assumes LB handles port exposure)
docker compose up -d --scale controller=3
```

> The included `docker-compose.yaml` is for development and exposes fixed ports.
> For HA testing, remove the `ports` from the controller service and put a reverse proxy (nginx etc.) in front.

### Kubernetes (conceptual example)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: unified-cd-controller
spec:
  replicas: 3
  selector:
    matchLabels: { app: unified-cd-controller }
  template:
    metadata:
      labels: { app: unified-cd-controller }
    spec:
      containers:
        - name: controller
          image: your-registry/unified-cd-controller:latest
          args: ["--addr", ":8080"]
          ports: [{ containerPort: 8080 }]
          env:
            - name: UNIFIED_DB_DSN
              valueFrom: { secretKeyRef: { name: unified-cd, key: db-dsn } }
            - name: UNIFIED_CONTROLLER_KEY       # same on all replicas, injected from Secret
              valueFrom: { secretKeyRef: { name: unified-cd, key: controller-key } }
            - name: UNIFIED_TOKEN
              valueFrom: { secretKeyRef: { name: unified-cd, key: admin-token } }
            - name: UNIFIED_S3_ENDPOINT
              value: s3.amazonaws.com
            # UNIFIED_S3_BUCKET / KEY / SECRET etc. similarly
          livenessProbe:
            httpGet: { path: /healthz, port: 8080 }
            periodSeconds: 10
          readinessProbe:
            httpGet: { path: /readyz, port: 8080 }
            periodSeconds: 10
      terminationGracePeriodSeconds: 30   # drain grace period (>= 12s)
---
apiVersion: v1
kind: Service
metadata:
  name: unified-cd-controller
spec:
  selector: { app: unified-cd-controller }
  ports: [{ port: 80, targetPort: 8080 }]
```

> Controller replicas handle API availability and scheduling. Run execution capacity
> is scaled separately via the number of agents.

---

## Failure Scenarios and Behavior

| Failure | Behavior |
|---------|----------|
| A non-leader controller goes down | LB stops routing to it (fails `/healthz`). Other replicas continue. No impact. |
| The **leader** controller goes down | Advisory lock is released, a follower becomes leader on the next tick. Scheduling pauses briefly. |
| PostgreSQL primary fails → failover | All controllers get temporary DB errors. `/readyz` returns 503 and they are rotated out. Auto-reconnect and leader re-election after promotion. |
| S3 failure | API continues. Log archive/retrieval temporarily fails (leader retries). In-progress Runs continue on agents. |
| All controllers down | In-progress Runs continue on agents, but results cannot be reported and claiming stops. Resumes after controller recovery. |
| Deploy (rolling) | One replica at a time: drain → stop → start new version. Zero-downtime via `/healthz` 503 drain. |

---

## HA Checklist

- [ ] Controller is running on 2+ instances behind an L7 load balancer
- [ ] **`UNIFIED_CONTROLLER_KEY` is identical on all replicas** (auto-synced from shared DB if unset, but explicit is recommended)
- [ ] `UNIFIED_TOKEN` / `UNIFIED_S3_*` / `UNIFIED_OIDC_*` are consistent across all replicas
- [ ] PostgreSQL is in an HA configuration (managed service or Patroni etc.)
- [ ] If using a pooler, it is in **session pooling mode** (required for advisory locks and NOTIFY)
- [ ] S3 is managed or distributed Garage
- [ ] LB health check is pointed at `/healthz` (readiness at `/readyz`)
- [ ] LB idle timeout is long enough for SSE connections
- [ ] Sticky sessions are **disabled** (not needed)
- [ ] Multiple agents are running in the same pool
- [ ] Rolling deploy with zero downtime has been verified

---

See also: [Authentication Guide (SSO / non-SSO)](authentication.md)
