# HA Hard-Failure Lock-Release Test — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Verify that with tuned PostgreSQL TCP keepalives, a hard network partition of the scheduler leader (via `docker network disconnect`, no FIN) releases its advisory lock and a follower takes over within a bound (≤ 20 s), not PostgreSQL's default (~2 h).

**Architecture:** A minimal `//go:build ha` docker-compose stack (tuned Postgres + 2 controllers) driven by a Go test that partitions the leader off the DB network and measures failover. Reuses the docker-driver style already proven in `test/ha/ha_test.go`.

**Tech Stack:** Go 1.26, Docker Compose v2, `docker/controller.Dockerfile`, `controller.RunScheduler` (logs `scheduler became leader`).

## Global Constraints

- Go module `github.com/eirueimi/unified-cd`, Go 1.26.2.
- Spec: `docs/superpowers/specs/2026-07-03-ha-hardfailure-design.md`.
- `//go:build ha` (excluded from normal `go test`; run via `go test -tags ha ./test/ha/` / `make ha-test`). SKIP if Docker unavailable. Teardown (`down -v`) via `t.Cleanup` even on failure.
- Partition method: `docker network disconnect <network> <leader-container>` (real TCP blackhole, no FIN). Do NOT use toxiproxy application toxics (they leave the TCP socket alive → keepalives answered → wrong path).
- PG keepalives: `-c tcp_keepalives_idle=5 -c tcp_keepalives_interval=2 -c tcp_keepalives_count=2` (~9 s detection).
- Bound: failover (surviving controller becomes leader AND queues a new Pending run) within ≤ 20 s of the disconnect. Fail otherwise.
- Controller env matches the merged failover stack: `UNIFIED_DB_DSN`, `UNIFIED_TOKEN=ha-admin-token` (also the agent/admin token), `UNIFIED_CONTROLLER_KEY`. Controllers listen on `:8080`. Admin API auth: `Authorization: Bearer ha-admin-token`.
- API shapes (validated in the merged HA work): apply job `POST /api/v1/jobs` `{"yaml":"..."}`; trigger `POST /api/v1/runs` `{"jobName":"..."}` → `{"id":"..."}`; poll `GET /api/v1/runs/{id}` → `"status":"Queued"|...`; jobs need `agentSelector: [kind:linux]` only if an agent must claim — but this test does NOT run agents, so a run reaching **Queued** is the success signal (no agent needed).

---

## Task 1: Hard-failure stack + partition driver + docs

**Files:**
- Create: `test/ha/docker-compose.hardfail.yaml`
- Create: `test/ha/hardfailure_test.go` (`//go:build ha`, `package ha`)
- Modify: `docs/high-availability.md` (§Hard failure mitigation note)

**Interfaces:**
- The new test file is in the same `package ha` as `test/ha/ha_test.go`; it may reuse trivially-compatible helpers there (e.g. a docker-availability check) but must use its OWN compose file + its OWN host ports (do not disturb `ha_test.go` or its constants).

- [ ] **Step 1: Compose stack**

Create `test/ha/docker-compose.hardfail.yaml` — tuned Postgres on an explicit network, two controllers with distinct host ports (so the test can talk to the survivor), no nginx/agents:

```yaml
name: unified-cd-hardfail
networks:
  dbnet:
    name: unified-cd-hardfail-dbnet
services:
  postgres:
    image: postgres:16-alpine
    command:
      - "postgres"
      - "-c"
      - "tcp_keepalives_idle=5"
      - "-c"
      - "tcp_keepalives_interval=2"
      - "-c"
      - "tcp_keepalives_count=2"
    environment:
      POSTGRES_USER: unified
      POSTGRES_PASSWORD: unified
      POSTGRES_DB: unified
    networks: [dbnet]
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
      UNIFIED_CONTROLLER_KEY: 00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff
    networks: [dbnet]
    ports:
      - "18081:8080"
    depends_on:
      postgres: { condition: service_healthy }
  controller2:
    <<: *ctrl
    ports:
      - "18082:8080"
```

Notes: the explicit network `unified-cd-hardfail-dbnet` is the disconnect target. Host ports (`18081`/`18082`) are independent of the network membership, so a controller disconnected from `dbnet` is still reachable from the host for logs/health, but its PG connection is blackholed. (Verify the controller reads `UNIFIED_CONTROLLER_KEY`; adjust if the env name differs — the merged stack proved DSN/token names.)

- [ ] **Step 2: Write the driver (failing until the stack works)**

Create `test/ha/hardfailure_test.go`:

```go
//go:build ha

package ha

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	hfCompose = "docker-compose.hardfail.yaml"
	hfNetwork = "unified-cd-hardfail-dbnet"
	hfBound   = 20 * time.Second
)

// hfCompose helpers (self-contained; do not reuse ha_test.go's ha-stack constants).
func hf(t *testing.T, args ...string) []byte {
	t.Helper()
	out, err := exec.Command("docker", append([]string{"compose", "-f", hfCompose}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose %v: %v\n%s", args, err, out)
	}
	return out
}

func hfLeader(t *testing.T) string {
	// Return "controller1" or "controller2" — the one whose logs most recently show
	// "scheduler became leader". Poll up to ~15s for a leader to appear.
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, svc := range []string{"controller1", "controller2"} {
			logs := hf(t, "logs", svc)
			if strings.Contains(string(logs), "scheduler became leader") {
				return svc
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no scheduler leader elected within 15s")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func hfPort(svc string) string {
	if svc == "controller1" {
		return "18081"
	}
	return "18082"
}

func hfPost(t *testing.T, port, path string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "http://localhost:"+port+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer ha-admin-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, rb
}

func hfTriggerQueued(t *testing.T, port string, within time.Duration) (string, time.Duration) {
	// Trigger a run via `port` and poll until it reaches Queued; return (runID, elapsed).
	t.Helper()
	start := time.Now()
	var runID string
	deadline := time.Now().Add(within)
	// trigger (retry submission — the survivor may briefly 5xx right after partition)
	for runID == "" {
		code, rb := hfPost(t, port, "/api/v1/runs", map[string]string{"jobName": "hf"})
		if code == 200 || code == 201 {
			var r struct{ ID string `json:"id"` }
			_ = json.Unmarshal(rb, &r)
			runID = r.ID
		}
		if runID == "" && time.Now().After(deadline) {
			t.Fatalf("could not trigger a run via %s within %s", port, within)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// poll status
	for {
		req, _ := http.NewRequest("GET", "http://localhost:"+port+"/api/v1/runs/"+runID, nil)
		req.Header.Set("Authorization", "Bearer ha-admin-token")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			rb, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(rb), `"status":"Queued"`) ||
				strings.Contains(string(rb), `"status":"Running"`) ||
				strings.Contains(string(rb), `"status":"Succeeded"`) {
				return runID, time.Since(start)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not reach Queued within %s", runID, within)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestHA_HardFailureLockRelease(t *testing.T) {
	if exec.Command("docker", "version").Run() != nil {
		t.Skip("docker not available")
	}
	hf(t, "up", "-d", "--build")
	t.Cleanup(func() {
		_, _ = exec.Command("docker", "compose", "-f", hfCompose, "down", "-v").CombinedOutput()
	})

	// Wait for readiness on both controllers.
	waitHFReady(t, "18081", 120*time.Second)
	waitHFReady(t, "18082", 120*time.Second)

	// Apply a job (no agent needed; success signal is Queued).
	code, rb := hfPost(t, "18081", "/api/v1/jobs",
		map[string]string{"yaml": "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: hf\nspec:\n  steps:\n    - name: s\n      run: echo ok\n"})
	if code != 200 && code != 201 {
		t.Fatalf("apply job: %d %s", code, rb)
	}

	// Baseline: current leader queues a run.
	leader := hfLeader(t)
	t.Logf("leader=%s", leader)
	_, _ = hfTriggerQueued(t, hfPort(leader), 10*time.Second)

	// HARD PARTITION: disconnect the leader from the DB network (no FIN).
	survivor := "controller2"
	if leader == "controller2" {
		survivor = "controller1"
	}
	t0 := time.Now()
	out, err := exec.Command("docker", "network", "disconnect", hfNetwork, containerName(t, leader)).CombinedOutput()
	if err != nil {
		t.Fatalf("network disconnect: %v\n%s", err, out)
	}
	t.Logf("partitioned leader=%s at t0; measuring failover via survivor=%s", leader, survivor)

	// Survivor must take over: a NEW run reaches Queued within the bound.
	_, elapsed := hfTriggerQueued(t, hfPort(survivor), hfBound)
	total := time.Since(t0)
	t.Logf("hard-failure failover: new run Queued via survivor in %s (measured from partition: %s)", elapsed, total)
	if total > hfBound {
		t.Fatalf("hard-failure lock release/failover took %s (> bound %s) — keepalives not effective?", total, hfBound)
	}
}
```

Add the two remaining helpers:
- `waitHFReady(t, port, within)` — poll `GET http://localhost:{port}/readyz` until 200.
- `containerName(t, svc)` — resolve the actual container name for a compose service (`docker compose -f hfCompose ps -q <svc>` gives the container ID, which `docker network disconnect` accepts; use the ID). Prefer the container ID from `ps -q` over guessing the name.

- [ ] **Step 3: Bring it up and iterate to green**

Run: `cd test/ha && go test -tags ha -run TestHA_HardFailureLockRelease -v -timeout 15m ./...`
Expected: PASS — after the leader is disconnected from `dbnet`, PG detects the dead session via keepalives (~9 s) and releases the scheduler lock; the survivor becomes leader and queues the new run within ≤ 20 s. Iterate:
- If the disconnect target is wrong, fix `containerName` to use `docker compose ps -q <svc>`.
- If failover exceeds 20 s, first confirm PG actually applied the keepalive settings (`docker compose exec postgres sh -c 'psql -U unified -c "show tcp_keepalives_idle"'`); only widen the bound if the measured, correctly-tuned time genuinely needs it — and record why.
- Ensure teardown leaves no containers (`docker ps`) and removes the `unified-cd-hardfail-dbnet` network.

- [ ] **Step 4: Document the verified bound**

In `docs/high-availability.md` §"Hard failure mitigation", add a sentence: this tuned-keepalive bound is now verified by `test/ha/hardfailure_test.go` (a `docker network disconnect` partition of the leader releases the advisory lock within ~keepalive-detection time); WITHOUT the `tcp_keepalives_*` tuning, release falls back to PostgreSQL's default (~2 h), so the tuning is REQUIRED for bounded hard-failure failover. Keep it concise and match the section's style.

- [ ] **Step 5: Verify + commit**

- `go build ./...` and `go vet -tags ha ./test/ha/` clean.
- `go test ./... -short` unaffected (build-tagged out).

```bash
git add test/ha/docker-compose.hardfail.yaml test/ha/hardfailure_test.go docs/high-availability.md
git commit -m "test(ha): verify hard-failure advisory-lock release via network partition + tuned keepalives"
```

---

## Final verification

- [ ] `go build -tags ha ./test/ha/` compiles; `go vet -tags ha ./test/ha/` clean.
- [ ] `cd test/ha && go test -tags ha -run TestHA_HardFailureLockRelease -v -timeout 15m ./...` passes; measured failover recorded in the log (expect ~9–15 s).
- [ ] `docker ps` clean afterward; `unified-cd-hardfail-dbnet` network removed.
- [ ] `go test ./... -short` still green.

## Self-review notes (coverage vs spec)

- Hard partition via `docker network disconnect` (no FIN) → Task 1 Step 2 (partition) + Step 3.
- Tuned PG keepalives (~9 s) → Task 1 Step 1.
- 2-controller minimal stack, distinct host ports, no nginx/agents → Task 1 Step 1.
- Failover observed via leader log + new run reaching Queued (no agent needed) → Task 1 Step 2.
- Bound ≤ 20 s, fail otherwise (falsifier: unreleased lock → new run stuck Pending → timeout) → Task 1 Step 2/3.
- Doc note (verified bound + keepalive requirement) → Task 1 Step 4.
- `//go:build ha`, docker-skip, teardown-always → Task 1 Step 2.
