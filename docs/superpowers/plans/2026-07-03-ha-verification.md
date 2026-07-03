# High-Availability Verification Tests — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Verify unified-cd's HA claims with (1) deterministic Postgres-backed Go tests for the primitives that lack system-level coverage, and (2) a `//go:build ha` Docker-Compose harness that kills the controller leader and asserts no double execution, no lost runs, fast failover, and API availability.

**Architecture:** Level 1 adds white-box Go tests in `internal/store` and `internal/controller` using the existing `NewTestPostgres(t)` dockertest helper. Level 2 adds a `test/ha/` stack (3 controllers + nginx + 2 agents + Postgres) driven by a build-tagged Go test that manipulates it via `docker compose` + the HTTP API.

**Tech Stack:** Go 1.26, pgx/pgxpool, testify, dockertest (Level 1), Docker Compose v2 + nginx (Level 2). The controller image is `docker/controller.Dockerfile` (distroless, entrypoint `/controller`, reads `UNIFIED_*` env). The standard agent is `cmd/agent` (flags `--server`/`--token`/`--labels`).

## Global Constraints

- Go module `github.com/eirueimi/unified-cd`, Go 1.26.2.
- Spec: `docs/superpowers/specs/2026-07-03-ha-verification-design.md`.
- Level 1 tests are white-box (`package store` / `package controller`), Postgres-backed via `NewTestPostgres(t)` (skips in `-short`), deterministic, complete in seconds.
- Level 2 is tagged `//go:build ha` (excluded from normal `go test`; run via `go test -tags ha ./test/ha/`); it must skip with a clear message when Docker is unavailable, and tear the stack down (`docker compose down -v`) even on failure.
- Level 2 uses the **standard containerized agent** (not k8s-agent); **omit S3/Garage**.
- Failover threshold ≤ 10 s; invariants: 0 double executions, 0 lost runs, no sustained 5xx.
- The claim method under test is `store.ClaimNextRun(ctx, agentID string, agentLabels []string) (*ClaimedRun, error)`; the scheduler is `controller.RunScheduler(ctx, st, tick)`; advisory lock via `store.AcquireAdvisoryLock(ctx, key int64) (func(), error)` (returns `(nil, nil)` when held elsewhere).

---

## File map

| Path | Responsibility |
|---|---|
| `internal/store/postgres_ha_test.go` | Level 1: no-double-claim + advisory-lock-release-on-conn-close |
| `internal/controller/scheduler_ha_test.go` | Level 1: scheduler failover |
| `test/ha/docker-compose.ha.yaml` | Level 2 stack (postgres, 3 controllers, nginx, 2 agents) |
| `test/ha/nginx.conf` | Level 2 LB config |
| `docker/agent.Dockerfile` | Containerized standard agent (with a shell for `run:` steps) |
| `test/ha/ha_test.go` | Level 2 `//go:build ha` driver + invariant assertions |
| `Makefile` | `ha-test` target |

---

## Task 1: Level 1 — store HA tests (no double claim, lock release on crash)

**Files:**
- Create: `internal/store/postgres_ha_test.go` (`package store`)

**Interfaces:**
- Consumes: `NewTestPostgres(t) *Postgres`; `pg.UpsertJob`, `pg.CreateRun`, `pg.TransitionPendingToQueued`, `pg.ClaimNextRun`, `pg.AcquireAdvisoryLock`; white-box field `pg.pool *pgxpool.Pool`.

- [ ] **Step 1: Write the failing tests**

Create `internal/store/postgres_ha_test.go`:

```go
package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHA_NoDoubleClaim verifies that concurrent claimers never claim the same
// run twice (FOR UPDATE SKIP LOCKED conflict-free claiming).
func TestHA_NoDoubleClaim(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	const runs = 50
	const claimers = 8

	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	spec := []byte(`{"steps":[{"name":"s","run":"echo x"}]}`)
	for i := 0; i < runs; i++ {
		_, err := pg.CreateRun(ctx, "j", nil, spec, nil, "")
		require.NoError(t, err)
	}
	// Move all Pending -> Queued so ClaimNextRun can pick them up.
	n, err := pg.TransitionPendingToQueued(ctx, runs)
	require.NoError(t, err)
	require.Equal(t, runs, n)

	var mu sync.Mutex
	claimedBy := map[string]int{} // runID -> number of times claimed

	var wg sync.WaitGroup
	for c := 0; c < claimers; c++ {
		wg.Add(1)
		go func(agentIdx int) {
			defer wg.Done()
			agentID := "agent-" + string(rune('a'+agentIdx))
			for {
				claimed, err := pg.ClaimNextRun(ctx, agentID, nil)
				if err != nil {
					t.Errorf("claim error: %v", err)
					return
				}
				if claimed == nil {
					return // queue drained
				}
				mu.Lock()
				claimedBy[claimed.ID]++
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()

	require.Len(t, claimedBy, runs, "every run should be claimed exactly once")
	for id, count := range claimedBy {
		assert.Equal(t, 1, count, "run %s claimed %d times (double claim!)", id, count)
	}
}

// TestHA_AdvisoryLockReleasedOnConnClose verifies PostgreSQL auto-releases a
// session-level advisory lock when the holder's connection dies abruptly
// (the crash-failover path, not a graceful unlock).
func TestHA_AdvisoryLockReleasedOnConnClose(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	const key = int64(0x68617465) // 'hate' — test-only key, distinct from prod keys

	// Open a standalone connection (not from pg's pool) to the same DB and hold the lock.
	dsn := pg.pool.Config().ConnString()
	raw, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	_, err = raw.Exec(ctx, "SELECT pg_advisory_lock($1)", key)
	require.NoError(t, err)

	// While held, pg cannot acquire it.
	rel, err := pg.AcquireAdvisoryLock(ctx, key)
	require.NoError(t, err)
	require.Nil(t, rel, "lock is held by the standalone connection")

	// Simulate a crash: close the underlying network connection abruptly.
	require.NoError(t, raw.PgConn().Conn().Close())

	// PostgreSQL should release the lock on session end; another acquire succeeds.
	deadline := time.Now().Add(5 * time.Second)
	for {
		rel, err := pg.AcquireAdvisoryLock(ctx, key)
		require.NoError(t, err)
		if rel != nil {
			rel()
			return // released, as expected
		}
		if time.Now().After(deadline) {
			t.Fatal("advisory lock was not released within 5s of connection close")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail (or reveal API mismatches)**

Run: `go test ./internal/store/ -run 'TestHA_' -v`
Expected: FAIL/compile-error if any assumption is off (e.g. `ClaimNextRun` requires matching labels, or `CreateRun` signature differs). Fix by matching the real signatures: confirm `CreateRun` arg order (`jobName, params, spec, agentSelector, triggeredBy`) and that a `nil` agentSelector run is claimable by an agent with `nil` labels. If `nil` labels don't match, create the runs with an explicit selector and pass matching labels to `ClaimNextRun`.

- [ ] **Step 3: Make them pass**

Adjust only as needed to match real signatures (the logic is correct). Re-run until green. The tests spin two Postgres containers (one per test) via dockertest.

Run: `go test ./internal/store/ -run 'TestHA_' -v`
Expected: PASS (both). If `-race` is available, also run `go test -race ./internal/store/ -run TestHA_NoDoubleClaim` — the concurrent claimers must be race-clean.

- [ ] **Step 4: Commit**

```bash
git add internal/store/postgres_ha_test.go
git commit -m "test(ha): store-level no-double-claim and advisory-lock-release-on-crash"
```

---

## Task 2: Level 1 — scheduler failover test

**Files:**
- Create: `internal/controller/scheduler_ha_test.go` (`package controller`)

**Interfaces:**
- Consumes: `store.NewTestPostgres(t)`; `RunScheduler(ctx, st, tick)`; `pg.UpsertJob`, `pg.CreateRun`, `pg.GetRun`.
- Note: `RunScheduler` acquires the scheduler advisory lock once and holds it for the goroutine's lifetime; on ctx cancel the `defer release()` unlocks it.

- [ ] **Step 1: Write the failing test**

Create `internal/controller/scheduler_ha_test.go`:

```go
package controller

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/require"
)

// waitQueued polls until the run reaches Queued (or fails the test after timeout).
func waitQueued(t *testing.T, pg *store.Postgres, runID string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		run, err := pg.GetRun(context.Background(), runID)
		require.NoError(t, err)
		if run.Status == api.RunQueued {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s not Queued within %s (status=%s)", runID, within, run.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestHA_SchedulerFailover verifies that when the scheduler leader goes down,
// a surviving replica takes over and no pending runs are lost.
func TestHA_SchedulerFailover(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	// Replica A becomes leader and queues a pending run.
	ctxA, cancelA := context.WithCancel(context.Background())
	go RunScheduler(ctxA, pg, 50*time.Millisecond)

	runA, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	waitQueued(t, pg, runA.ID, 3*time.Second) // A is leader

	// Leader A goes down -> its advisory lock is released.
	cancelA()

	// Replica B takes over and queues a new pending run (no run lost).
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	go RunScheduler(ctxB, pg, 50*time.Millisecond)

	runB, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	waitQueued(t, pg, runB.ID, 5*time.Second) // B took over
}
```

- [ ] **Step 2: Run test to verify it fails (or reveal API mismatches)**

Run: `go test ./internal/controller/ -run TestHA_SchedulerFailover -v`
Expected: FAIL/compile-error if `GetRun`/`CreateRun`/`RunScheduler` signatures differ; fix to match. Confirm `api.RunQueued` is the queued-status constant.

- [ ] **Step 3: Make it pass**

Adjust to real signatures; re-run until green.

Run: `go test ./internal/controller/ -run TestHA_SchedulerFailover -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/scheduler_ha_test.go
git commit -m "test(ha): scheduler leader failover and no-lost-runs"
```

---

## Task 3: Level 2 — the failover stack (compose + nginx + agent image)

**Files:**
- Create: `test/ha/docker-compose.ha.yaml`, `test/ha/nginx.conf`, `docker/agent.Dockerfile`

**Interfaces:**
- Produces: a `docker compose` stack that brings up Postgres, 3 controllers, an nginx LB (single exposed port), and 2 agents — all healthy, able to run a job end-to-end. Task 4's Go driver consumes this compose file by path.

This task is infra: iterate against a live `docker compose up` until the stack is healthy and a submitted job Succeeds. Verification is manual/scripted (no Go test yet).

- [ ] **Step 1: Agent Dockerfile**

The standard agent execs `run:` steps via a shell, so its image needs a shell (the controller's distroless base does not). Create `docker/agent.Dockerfile`:

```dockerfile
# Stage 1: build the agent binary
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /agent ./cmd/agent

# Stage 2: runtime with a shell (bash) for run: steps
FROM alpine:3.20
RUN apk add --no-cache bash coreutils
COPY --from=build /agent /usr/local/bin/agent
ENTRYPOINT ["agent"]
```

- [ ] **Step 2: nginx LB config**

Create `test/ha/nginx.conf` — round-robin over the 3 controllers, pass through, generous timeouts for long-poll/SSE:

```nginx
events {}
http {
  upstream controllers {
    server controller1:8080;
    server controller2:8080;
    server controller3:8080;
  }
  server {
    listen 8080;
    location / {
      proxy_pass http://controllers;
      proxy_http_version 1.1;
      proxy_set_header Connection "";
      proxy_read_timeout 300s;
      proxy_send_timeout 300s;
      proxy_next_upstream error timeout http_502 http_503 http_504;
    }
  }
}
```

- [ ] **Step 3: Compose stack**

Create `test/ha/docker-compose.ha.yaml`. Three controllers built from `docker/controller.Dockerfile`, identical env, no host ports; nginx exposes `18080`; two agents point at nginx. Build context is the repo root (`../..`).

```yaml
name: unified-cd-ha
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: unified
      POSTGRES_PASSWORD: unified
      POSTGRES_DB: unified
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U unified"]
      interval: 2s
      timeout: 3s
      retries: 30

  controller1: &ctrl
    build:
      context: ../..
      dockerfile: docker/controller.Dockerfile
    environment:
      UNIFIED_DB_DSN: postgres://unified:unified@postgres:5432/unified?sslmode=disable
      UNIFIED_TOKEN: ha-admin-token
      UNIFIED_AGENT_TOKEN: ha-agent-token
      UNIFIED_CONTROLLER_KEY: 00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff
    depends_on:
      postgres: { condition: service_healthy }
  controller2: *ctrl
  controller3: *ctrl

  nginx:
    image: nginx:1.27-alpine
    volumes:
      - ./nginx.conf:/etc/nginx/nginx.conf:ro
    ports:
      - "18080:8080"
    depends_on: [controller1, controller2, controller3]

  agent1: &agent
    build:
      context: ../..
      dockerfile: docker/agent.Dockerfile
    command: ["--server", "http://nginx:8080", "--token", "ha-agent-token", "--labels", "kind:linux"]
    depends_on: [nginx]
  agent2: *agent
```

(Verify the controller's DB DSN env var name is `UNIFIED_DB_DSN` and the admin/agent token env names against `internal/config` — adjust if the real names differ. Confirm the controller listens on `:8080` by default; if it needs an `--addr`/`UNIFIED_ADDR`, add it. Confirm the agent token flag/env and label-selector format the sample jobs will use.)

- [ ] **Step 4: Verify the stack end-to-end (manual/scripted)**

From `test/ha/`:

```bash
docker compose -f docker-compose.ha.yaml up -d --build
# wait for health, then through the LB:
curl -sf http://localhost:18080/readyz
# submit a trivial job and run via the API using UNIFIED_TOKEN=ha-admin-token
# (apply a job with a `run: echo ok` step, trigger it, poll until Succeeded)
docker compose -f docker-compose.ha.yaml logs controller1 | grep -i "scheduler became leader" || true
docker compose -f docker-compose.ha.yaml down -v
```

Expected: `/readyz` returns 200 via nginx; both agents register; a submitted job reaches `Succeeded`; exactly one controller logs `scheduler became leader`. Iterate on the Dockerfiles/compose/env until this holds. Document the exact working `curl`/CLI commands you used in the task report (Task 4 automates them).

- [ ] **Step 5: Commit**

```bash
git add test/ha/docker-compose.ha.yaml test/ha/nginx.conf docker/agent.Dockerfile
git commit -m "test(ha): docker-compose failover stack (3 controllers, nginx, 2 agents)"
```

---

## Task 4: Level 2 — the failover driver + assertions

**Files:**
- Create: `test/ha/ha_test.go` (`//go:build ha`)
- Modify: `Makefile` (add `ha-test`)

**Interfaces:**
- Consumes: the stack from Task 3 (compose file path, nginx port `18080`, `UNIFIED_TOKEN=ha-admin-token`, controller service names `controller1..3`, the `scheduler became leader` log line).

- [ ] **Step 1: Write the driver skeleton (build-tagged, Docker-guarded, self-cleaning)**

Create `test/ha/ha_test.go`:

```go
//go:build ha

package ha

import (
	"os/exec"
	"testing"
	"time"
)

const (
	composeFile = "docker-compose.ha.yaml"
	baseURL     = "http://localhost:18080"
	adminToken  = "ha-admin-token"
)

func dockerAvailable() bool {
	return exec.Command("docker", "version").Run() == nil
}

func compose(t *testing.T, args ...string) []byte {
	t.Helper()
	out, err := exec.Command("docker", append([]string{"compose", "-f", composeFile}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose %v: %v\n%s", args, err, out)
	}
	return out
}

func TestHA_ControllerFailover(t *testing.T) {
	if !dockerAvailable() {
		t.Skip("docker not available")
	}
	compose(t, "up", "-d", "--build")
	t.Cleanup(func() {
		_, _ = exec.Command("docker", "compose", "-f", composeFile, "down", "-v").CombinedOutput()
	})

	waitReady(t, 120*time.Second) // poll baseURL/readyz until 200

	// ... the phases below
}
```

- [ ] **Step 2: Add the helpers and workload**

In the same file, implement (complete code, no placeholders):
- `waitReady(t, within)` — poll `GET {baseURL}/readyz` until 200.
- `apiPost/apiGet(t, path, body)` — HTTP with `Authorization: Bearer {adminToken}`; return status + body.
- `currentLeader(t) string` — return the controller service name (`controller1..3`) whose logs contain the LAST `scheduler became leader` line: `docker compose -f ... logs <svc>` and grep; the leader is the one that logged it most recently and hasn't since lost it. (Simplest robust approach: for each of the 3, count `scheduler became leader` occurrences and check it's still running; the current leader is the running controller that most recently logged the line. If ambiguous, submit a schedule/pending run and see which controller's log shows the queueing.)
- `submitRuns(t, n) []string` — apply a job (`{"steps":[{"name":"s","run":"sleep 1"}]}`) once, trigger it `n` times through the LB, return run IDs.
- `waitAllSucceeded(t, ids, within)` — poll each run until `Succeeded`; fail on timeout or any non-success terminal state.
- `countServerErrors` — a background poller struct that hits `GET {baseURL}/readyz` (or `/api/v1/jobs`) every 200ms during a window and records any 5xx.

- [ ] **Step 3: Implement the failover phases + invariant assertions**

In `TestHA_ControllerFailover`, after `waitReady`:

```go
	// Phase 0: baseline workload completes.
	ids := submitRuns(t, 10)
	waitAllSucceeded(t, ids, 90*time.Second)

	// Phase 1: kill a NON-leader -> no disruption.
	leader := currentLeader(t)
	nonLeader := anyOtherController(leader)
	compose(t, "kill", nonLeader)
	ids = submitRuns(t, 5)
	waitAllSucceeded(t, ids, 90*time.Second)

	// Phase 2: graceful stop of the LEADER (SIGTERM) -> new leader, fast failover.
	compose(t, "up", "-d", nonLeader) // restore so we still have 3
	leader = currentLeader(t)
	errPoll := startErrorPoller(t)
	compose(t, "stop", leader)
	assertFailoverUnder(t, 10*time.Second) // a newly-submitted run reaches Queued within 10s
	ids = submitRuns(t, 5)
	waitAllSucceeded(t, ids, 90*time.Second)

	// Phase 3: crash the LEADER (SIGKILL) -> new leader, fast failover.
	compose(t, "up", "-d", leader) // restore to 3
	leader = currentLeader(t)
	compose(t, "kill", leader)
	assertFailoverUnder(t, 10*time.Second)
	ids = submitRuns(t, 5)
	waitAllSucceeded(t, ids, 90*time.Second)

	// Invariants across the whole run:
	errPoll.stop()
	assert5xxWithinBound(t, errPoll, 2)    // no sustained 5xx (allow brief transient)
	assertNoDoubleExecution(t)             // each run executed exactly once
```

Implement the assertion helpers:
- `assertFailoverUnder(t, d)` — submit one run, measure time until it reaches `Queued` (or `Running`), fail if > d.
- `assertNoDoubleExecution(t)` — for every run created in the test, fetch its step reports (`GET /api/v1/runs/{id}/steps`) and assert each step index has exactly one `Succeeded` report (no run/step executed twice). Since the workload steps are idempotent (`sleep`), also assert the run count in the DB equals the number submitted (no phantom duplicates).
- `assert5xxWithinBound(t, poller, maxConsecutive)` — assert the poller never saw more than `maxConsecutive` consecutive 5xx (transient at the kill instant is tolerated; sustained failure is not).
- `anyOtherController(leader)` / `startErrorPoller` — straightforward helpers.

Write these as real, compiling Go. Prefer polling with clear timeouts over sleeps. Keep each helper small.

- [ ] **Step 4: Makefile target**

Add to `Makefile`:

```make
ha-test:
	cd test/ha && go test -tags ha -v -timeout 20m ./...
```

- [ ] **Step 5: Run the driver**

Run: `cd test/ha && go test -tags ha -v -timeout 20m ./...`
Expected: PASS — the stack builds, the three failover phases each elect a new leader within 10 s, all submitted runs Succeed exactly once, no sustained 5xx, and the stack is torn down. If Docker image builds are slow, the first run may take several minutes (building 2 images). If a phase is flaky due to timing, widen the failover threshold only up to the spec's 10 s and investigate rather than masking.

- [ ] **Step 6: Commit**

```bash
git add test/ha/ha_test.go Makefile
git commit -m "test(ha): docker-compose leader-failover driver and invariant assertions"
```

---

## Final verification

- [ ] Level 1: `go test ./internal/store/ ./internal/controller/ -run TestHA_ -v` — 3 tests pass (Postgres-backed).
- [ ] `go build ./...` and `go build -tags ha ./test/ha/` — both compile.
- [ ] Level 2 (if Docker present): `make ha-test` — passes with the invariants (0 double exec, 0 lost runs, failover ≤ 10 s, no sustained 5xx).
- [ ] `go test ./... -short` still green (Level 1 skips in `-short`; Level 2 is build-tagged out).

## Self-review notes (coverage vs spec)

- Spec Level-1 test 1 (no double claim) → Task 1 `TestHA_NoDoubleClaim`.
- Spec Level-1 test 2 (advisory lock release on conn close) → Task 1 `TestHA_AdvisoryLockReleasedOnConnClose`.
- Spec Level-1 test 3 (scheduler failover) → Task 2 `TestHA_SchedulerFailover`.
- Spec Level-2 stack (3 controllers + nginx + 2 agents, no S3) → Task 3.
- Spec Level-2 driver + failure matrix (non-leader kill, leader stop, leader kill) + invariants (0 double / 0 lost / ≤10 s / no sustained 5xx) → Task 4.
- Standard agent, S3 omitted, k8s-agent deferred → Tasks 3–4 (per Global Constraints).
- `make ha-test`, `//go:build ha`, Docker-skip → Task 4.
