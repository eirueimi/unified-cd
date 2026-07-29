# Edge-Case Campaign: Infrastructure + Wave W0 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the edge-case campaign skeleton (`test/edgecase/`) and execute Wave W0 (PgBouncer transaction-pooling probe + scheduler clock/TZ boundary probes), recording findings.

**Architecture:** Exploratory campaign per `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md`. W0-1 runs live against the existing `test/ha` compose stack plus a PgBouncer overlay. W0-2 runs as observational Go tests (build tag `edgeprobe`) against the real scheduler code with `store.NewTestPostgres`, because libfaketime cannot intercept a static Go binary (Go calls clock_gettime via vDSO, not libc) and Linux time namespaces do not virtualize CLOCK_REALTIME — container-level clock skew is not implementable without touching the host clock.

**Tech Stack:** docker compose (overlay on `test/ha/docker-compose.ha.yaml`), PgBouncer (`edoburu/pgbouncer`), Go tests with `store.NewTestPostgres` (dockertest-backed), curl against the nginx LB (`localhost:18080`, token `ha-admin-token`).

## Global Constraints

- All committed text is English (AGENTS.md).
- Work happens on branch `spec/edge-case-campaign` in worktree `wt-edge-spec` — never commit on the main checkout.
- **No production-code changes** during Phase 1 (spec §8). Test-only files (`test/edgecase/`, `//go:build edgeprobe` test files) are allowed.
- Probes are **observational**: they `t.Logf` observed behavior and only fail on infrastructure errors. Desired-behavior assertions come later, in Phase 2.
- Build tag `edgeprobe` keeps probes out of `go test ./...`.
- Docker must be running (compose stack and `NewTestPostgres` both need it).
- Later waves (W1+) get their own plans after the W0 checkpoint — do not build W1+ tooling (inject.sh, Garage, kind) in this plan.

---

### Task 1: Campaign skeleton + spec amendment

**Files:**
- Create: `test/edgecase/README.md`
- Create: `test/edgecase/FINDINGS.md`
- Modify: `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md`

**Interfaces:**
- Produces: `FINDINGS.md` entry format used by Tasks 4 and 6.

- [ ] **Step 1: Create `test/edgecase/README.md`**

```markdown
# Edge-Case Test Campaign

Exploratory testing of unified-cd's distributed-systems edge cases.
Spec: `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md`
(waves W0-W6, invariants I1-I7, findings workflow).

## Layout

- `FINDINGS.md` — one entry per invariant violation or notable observation.
- `scenarios/` — one runbook per scenario (`w<wave>-<n>-<slug>.md`).
- `compose/` — overlay files stacked onto `test/ha/docker-compose.ha.yaml`.
- `workloads/` — job/schedule YAML (and pre-encoded JSON API payloads).
- `probes/` — see note below.

## Running a compose scenario

Each runbook lists its exact stack invocation. The general shape:

    docker compose -f test/ha/docker-compose.ha.yaml \
      -f test/edgecase/compose/<overlay>.yaml up -d --build

The LB is `http://localhost:18080`, admin token `ha-admin-token`
(both inherited from the test/ha stack).

## Running probe tests

Scheduler/timing boundary probes are observational Go tests excluded from
normal builds by the `edgeprobe` build tag. They live next to the code they
probe (they call unexported functions), e.g.
`internal/controller/edgeprobe_scheduler_test.go`:

    go test -tags edgeprobe ./internal/controller -run TestEdgeProbe -v

Probes PASS unless infrastructure breaks; their `t.Logf` output is the
result. Copy notable output into `FINDINGS.md`.

## Rules

- Phase 1 is exploration: record findings, do NOT fix production code.
- Findings are reported in one batch after the final wave.
- Every scenario names the invariants (I1-I7) it attacks.
```

- [ ] **Step 2: Create `test/edgecase/FINDINGS.md`**

```markdown
# Campaign Findings

One entry per invariant violation or notable observation. Reported as one
batch at the end of the campaign; the operator prioritizes.

Severity: **critical** (data loss / silent corruption / security),
**major** (incorrect visible behavior, unbounded recovery),
**minor** (diagnosability, docs gap, cosmetic).

Entry template:

    ## <scenario-id> — <one-line title>
    - **Invariant:** I<n> (<name>)
    - **Severity:** critical | major | minor
    - **Repro:** <commands / probe name>
    - **Observed:** <what happened, with log/query excerpts>
    - **Expected:** <what the docs/spec promise>
    - **Notes:** <fix ideas, related known issues>

---

(no findings yet)
```

- [ ] **Step 3: Amend the spec's clock-skew tooling (libfaketime does not work on Go)**

In `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md`, replace the W0-2 table row:

```markdown
| W0-2 (C14) | Clock skew / TZ mismatch between controllers (libfaketime); DST fold; the fixed one-hour schedule catch-up window | I1 |
```

with:

```markdown
| W0-2 (C14) | Scheduler clock/TZ boundaries — TZ mismatch between replicas, DST gap/fold, one-hour catch-up window edges, backward clock step — probed via observational unit tests (build tag `edgeprobe`) driving `checkAndFireSchedules` with constructed `now` values. libfaketime cannot intercept static Go binaries (vDSO time calls) and Linux time namespaces do not cover CLOCK_REALTIME, so container-level skew injection is not implementable | I1 |
```

and replace the inject.sh description line:

```markdown
`clock-skew` (libfaketime baked into the test images — W0-2).
```

with:

```markdown
(clock-skew injection was dropped: libfaketime cannot intercept static Go
binaries and time namespaces do not virtualize CLOCK_REALTIME — W0-2 covers
clock boundaries via `edgeprobe` unit probes instead).
```

- [ ] **Step 4: Commit**

```bash
git add test/edgecase/README.md test/edgecase/FINDINGS.md docs/superpowers/specs/2026-07-29-edge-case-testing-design.md
git commit -m "test(edgecase): scaffold campaign skeleton; replace libfaketime with edgeprobe approach

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: W0 workloads + PgBouncer compose overlay

**Files:**
- Create: `test/edgecase/workloads/tick.yaml`
- Create: `test/edgecase/workloads/tick.payload.json`
- Create: `test/edgecase/workloads/schedule-every-minute.payload.json`
- Create: `test/edgecase/compose/pgbouncer.override.yaml`

**Interfaces:**
- Consumes: `test/ha/docker-compose.ha.yaml` service names (`postgres`, `controller1..3`, `nginx`, `agent1..2`), token `ha-admin-token`, LB port `18080`.
- Produces: overlay + payload files used verbatim by the Task 3 runbook.

- [ ] **Step 1: Create `test/edgecase/workloads/tick.yaml`** (human-readable reference; the API payload below embeds the same YAML)

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: edge-tick
spec:
  native: true
  agentSelector:
    - kind:linux
  steps:
    - name: tick
      run: for i in $(seq 1 30); do echo "tick $i"; sleep 1; done
```

`native: true` for the same reason as `test/ha/ha_test.go`'s workload — the compose agent image has no Docker daemon, so non-native runs fail at claim-pod construction.

- [ ] **Step 2: Create `test/edgecase/workloads/tick.payload.json`** (pre-encoded so runbooks need only curl, no jq/python)

```json
{"yaml":"apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: edge-tick\nspec:\n  native: true\n  agentSelector:\n    - kind:linux\n  steps:\n    - name: tick\n      run: for i in $(seq 1 30); do echo \"tick $i\"; sleep 1; done\n"}
```

- [ ] **Step 3: Create `test/edgecase/workloads/schedule-every-minute.payload.json`**

```json
{"yaml":"apiVersion: unified-cd/v1\nkind: Schedule\nmetadata:\n  name: edge-every-minute\nspec:\n  cron: \"* * * * *\"\n  job: edge-tick\n"}
```

- [ ] **Step 4: Create `test/edgecase/compose/pgbouncer.override.yaml`**

```yaml
# W0-1 overlay: route all controllers through PgBouncer in TRANSACTION
# pooling mode — the configuration docs/high-availability.md explicitly
# forbids (advisory locks and LISTEN/NOTIFY are session-level). The point
# is to observe HOW it breaks: diagnosable failure vs silent split-brain.
# Stack it onto the ha compose file; never use for other scenarios.
services:
  pgbouncer:
    image: edoburu/pgbouncer:latest
    environment:
      DB_HOST: postgres
      DB_USER: unified
      DB_PASSWORD: unified
      DB_NAME: unified
      POOL_MODE: transaction
      AUTH_TYPE: scram-sha-256
      LISTEN_PORT: "5432"
    depends_on:
      postgres:
        condition: service_healthy
  controller1:
    environment:
      UNIFIED_DB_DSN: postgres://unified:unified@pgbouncer:5432/unified?sslmode=disable
  controller2:
    environment:
      UNIFIED_DB_DSN: postgres://unified:unified@pgbouncer:5432/unified?sslmode=disable
  controller3:
    environment:
      UNIFIED_DB_DSN: postgres://unified:unified@pgbouncer:5432/unified?sslmode=disable
```

- [ ] **Step 5: Verify the merged compose config renders**

```bash
cd test/ha && docker compose -f docker-compose.ha.yaml -f ../edgecase/compose/pgbouncer.override.yaml config --quiet && echo MERGE-OK
```

Expected: `MERGE-OK` (no output before it). If it errors, fix YAML before committing.

- [ ] **Step 6: Commit**

```bash
git add test/edgecase/workloads/ test/edgecase/compose/pgbouncer.override.yaml
git commit -m "test(edgecase): add W0 workloads and pgbouncer transaction-pooling overlay

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: W0-1 runbook

**Files:**
- Create: `test/edgecase/scenarios/w0-1-pgbouncer-txn-pooling.md`

**Interfaces:**
- Consumes: Task 2's overlay and payload files, exactly as named there.

- [ ] **Step 1: Write the runbook**

````markdown
# W0-1 — PgBouncer in transaction-pooling mode

- **Invariants:** I1 (run accounting), I5 (bounded recovery — here: is the
  failure mode diagnosable?)
- **Stack:** test/ha compose + `compose/pgbouncer.override.yaml`
- **Docs contract:** `docs/high-availability.md` requires session pooling;
  transaction pooling "breaks advisory locks and NOTIFY". This scenario
  documents the actual failure mode an operator would see.

## Baseline (healthy stack, no overlay)

```bash
cd test/ha
docker compose -f docker-compose.ha.yaml up -d --build
curl -fsS localhost:18080/readyz            # expect: ok (retry until up)
curl -fsS -X POST localhost:18080/api/v1/jobs \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  --data-binary @../edgecase/workloads/tick.payload.json
curl -fsS -X POST localhost:18080/api/v1/schedules \
  -H "Authorization: Bearer ha-admin-token" -H "Content-Type: application/json" \
  --data-binary @../edgecase/workloads/schedule-every-minute.payload.json
```

Wait ~90s, then record the baseline:

```bash
# exactly one leader elected:
for c in controller1 controller2 controller3; do
  echo "== $c"; docker compose -f docker-compose.ha.yaml logs $c 2>/dev/null | grep -c "scheduler became leader"
done
# schedule fired (>=1 run):
curl -fsS "localhost:18080/api/v1/runs?jobName=edge-tick" -H "Authorization: Bearer ha-admin-token"
# SSE delivers log lines (replace <RUN_ID> from the previous output; expect
# "tick N" events streaming; Ctrl-C after a few):
curl -N "localhost:18080/api/v1/runs/<RUN_ID>/events" -H "Authorization: Bearer ha-admin-token"
```

Tear down INCLUDING volumes (schedule state must not leak into the probe):

```bash
docker compose -f docker-compose.ha.yaml down -v
```

## Probe (with PgBouncer overlay)

```bash
docker compose -f docker-compose.ha.yaml -f ../edgecase/compose/pgbouncer.override.yaml up -d --build
curl -fsS localhost:18080/readyz
```

Repeat the same job+schedule apply as the baseline. Then observe for ~5
minutes:

1. **Leader election:** same `grep -c "scheduler became leader"` loop as the
   baseline, plus `grep -i "advisory\|lock"` over all controller logs.
   Questions to answer: do multiple controllers claim leadership? Does
   leadership flap? Any unlock warnings ("you don't own a lock")?
2. **Scheduling:** does `edge-every-minute` fire at all / once per minute /
   multiple times per minute?
   `curl -fsS "localhost:18080/api/v1/runs?jobName=edge-tick" -H "Authorization: Bearer ha-admin-token"`
   Count runs after 5 minutes; expected-healthy would be ~5.
3. **SSE:** attach to a running run's `/events` — do log lines arrive
   (NOTIFY works) or does the stream stall?
4. **Advisory locks in PG:** which backend sessions hold them, and do they
   leak after a controller restart?
   ```bash
   docker compose -f docker-compose.ha.yaml exec postgres \
     psql -U unified -c "SELECT locktype, classid, objid, pid, granted FROM pg_locks WHERE locktype='advisory';"
   docker compose -f docker-compose.ha.yaml restart controller1
   # wait 60s, re-run the pg_locks query: are old locks still granted to dead sessions?
   ```
5. **Kill the apparent leader** (`docker compose ... kill controller<N>` for
   whichever logged leadership last): does another replica EVER become
   leader, or is the lock stranded on a PgBouncer server connection forever
   (scheduling halted = the split-brain-adjacent outcome the docs warn
   about)?

## Recording

FINDINGS entries (severity guidance): silent scheduling halt or duplicate
fires = **major**; noisy-but-clear errors pointing at pooling mode =
**minor** (docs-confirming). Also record whether any phantom/duplicate runs
appeared (I1) and the exact log lines an operator could alert on.

## Teardown

```bash
docker compose -f docker-compose.ha.yaml -f ../edgecase/compose/pgbouncer.override.yaml down -v
```
````

- [ ] **Step 2: Commit**

```bash
git add test/edgecase/scenarios/w0-1-pgbouncer-txn-pooling.md
git commit -m "test(edgecase): add W0-1 pgbouncer transaction-pooling runbook

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: Execute W0-1 and record findings

**Files:**
- Modify: `test/edgecase/FINDINGS.md`

**Interfaces:**
- Consumes: Task 3 runbook, verbatim.

- [ ] **Step 1: Execute the baseline section** — every command, in order. If the baseline itself is broken (no leader, schedule never fires, SSE dead), STOP and report: the probe is meaningless on a broken baseline.

- [ ] **Step 2: Execute the probe section** — every command, capturing output (`docker compose ... logs > /tmp-or-scratchpad` is fine; do not commit raw logs).

- [ ] **Step 3: Write FINDINGS entries** using the template from Task 1, one entry per distinct observation (expect 1–3: leader behavior, scheduling behavior, SSE behavior). Include exact log excerpts.

- [ ] **Step 4: Tear down the stack** (with `-v`), then commit:

```bash
git add test/edgecase/FINDINGS.md
git commit -m "test(edgecase): record W0-1 pgbouncer probe findings

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: W0-2 scheduler boundary probes

**Files:**
- Create: `internal/controller/edgeprobe_scheduler_test.go`

**Interfaces:**
- Consumes (all verified in current code): `store.NewTestPostgres(t) *store.Postgres`; `pg.UpsertJob(ctx, name, apiVersion string, spec []byte)`; `pg.UpsertSchedule(ctx, name, cron, jobName string, params map[string]string)`; `pg.UpdateScheduleLastFiredAt(ctx, name string, t time.Time)`; `pg.ListRunsByJob(ctx, jobName string, limit int) ([]api.Run, error)`; unexported `checkAndFireSchedules(ctx, st, now)` — fire window `next ∈ [now-1h, now]`, base = `last_fired_at` else `now-1h`; `dsl.NextCronTime(expr string, after time.Time) (time.Time, error)`.

- [ ] **Step 1: Write the probe file**

```go
//go:build edgeprobe

package controller

// Observational probes for scheduler clock/TZ boundary behavior (campaign
// scenario W0-2 / C14 — see docs/superpowers/specs/2026-07-29-edge-case-
// testing-design.md). These probes PASS unless infrastructure fails; the
// t.Logf output IS the result and is copied into test/edgecase/FINDINGS.md.
// Run: go test -tags edgeprobe ./internal/controller -run TestEdgeProbe -v

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/require"
)

func probeSetup(t *testing.T) *store.Postgres {
	t.Helper()
	pg := store.NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "probe-job", "unified-cd/v1",
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`))
	require.NoError(t, err)
	return pg
}

func countRuns(t *testing.T, pg *store.Postgres) int {
	t.Helper()
	runs, err := pg.ListRunsByJob(context.Background(), "probe-job", 100)
	require.NoError(t, err)
	return len(runs)
}

// TestEdgeProbe_DSTGap: "30 2 * * *" on the US spring-forward night —
// 02:30 does not exist on 2026-03-08 in America/New_York.
func TestEdgeProbe_DSTGap(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	base := time.Date(2026, 3, 8, 1, 0, 0, 0, ny)
	next, err := dsl.NextCronTime("30 2 * * *", base)
	require.NoError(t, err)
	t.Logf("DST gap: next('30 2 * * *') after %s = %s (skipped? %v)",
		base, next, next.Day() != 8)
}

// TestEdgeProbe_DSTFold: "30 1 * * *" on the US fall-back night —
// 01:30 occurs twice on 2026-11-01 in America/New_York.
func TestEdgeProbe_DSTFold(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, ny)
	first, err := dsl.NextCronTime("30 1 * * *", base)
	require.NoError(t, err)
	second, err := dsl.NextCronTime("30 1 * * *", first)
	require.NoError(t, err)
	t.Logf("DST fold: first=%s (%s) second=%s (%s) — fires twice on the fold night? %v",
		first, first.UTC(), second, second.UTC(), second.Sub(first) == time.Hour)
}

// TestEdgeProbe_CatchupWindowBoundary: the fire window is [now-1h, now].
// With cron */5, last_fired = now-65m puts next exactly AT now-60m
// (boundary), and last_fired = now-66m puts next just OUTSIDE it.
func TestEdgeProbe_CatchupWindowBoundary(t *testing.T) {
	pg := probeSetup(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	_, err := pg.UpsertSchedule(ctx, "probe-sched", "*/5 * * * *", "probe-job", nil)
	require.NoError(t, err)

	// Case A: next == now-60m exactly (on the window edge).
	require.NoError(t, pg.UpdateScheduleLastFiredAt(ctx, "probe-sched", now.Add(-65*time.Minute)))
	checkAndFireSchedules(ctx, pg, now)
	after := countRuns(t, pg)
	scA, err := pg.GetSchedule(ctx, "probe-sched")
	require.NoError(t, err)
	t.Logf("boundary next==now-1h: fired=%d last_fired_at=%v", after, scA.LastFiredAt)

	// Case B: next == now-61m (just outside) — expect silent advance, no run.
	require.NoError(t, pg.UpdateScheduleLastFiredAt(ctx, "probe-sched", now.Add(-66*time.Minute)))
	before := countRuns(t, pg)
	checkAndFireSchedules(ctx, pg, now)
	scB, err := pg.GetSchedule(ctx, "probe-sched")
	require.NoError(t, err)
	t.Logf("just outside window: new-runs=%d (silent skip? %v) last_fired_at=%v",
		countRuns(t, pg)-before, countRuns(t, pg) == before, scB.LastFiredAt)
}

// TestEdgeProbe_BacklogDrainRate: a 30-minute outage with cron */5 leaves 6
// due occurrences inside the window. checkAndFireSchedules computes ONE next
// per schedule per call — measure how many calls drain the backlog.
func TestEdgeProbe_BacklogDrainRate(t *testing.T) {
	pg := probeSetup(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	_, err := pg.UpsertSchedule(ctx, "probe-sched", "*/5 * * * *", "probe-job", nil)
	require.NoError(t, err)
	require.NoError(t, pg.UpdateScheduleLastFiredAt(ctx, "probe-sched", now.Add(-30*time.Minute)))

	for call := 1; call <= 8; call++ {
		checkAndFireSchedules(ctx, pg, now)
		t.Logf("call %d: total runs=%d", call, countRuns(t, pg))
	}
	// Production calls this once per minute: if each call fires one backlog
	// occurrence, a 30-min outage takes ~6 real minutes to drain.
}

// TestEdgeProbe_TZDivergentLeaders: replicas in different TZs. A UTC leader
// checks, then "fails over" to a JST leader one minute later, same instant
// stream. Cron interprets wall-clock in the Location carried by `now`/base.
func TestEdgeProbe_TZDivergentLeaders(t *testing.T) {
	pg := probeSetup(t)
	ctx := context.Background()
	jst, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)

	_, err = pg.UpsertSchedule(ctx, "probe-sched", "0 9 * * *", "probe-job", nil)
	require.NoError(t, err)

	// 2026-07-29 00:05 UTC == 09:05 JST: "daily at 09:00" is 5 minutes PAST
	// due for a JST replica and ~9h away for a UTC replica.
	nowUTC := time.Date(2026, 7, 29, 0, 5, 0, 0, time.UTC)
	checkAndFireSchedules(ctx, pg, nowUTC)
	afterUTC := countRuns(t, pg)
	checkAndFireSchedules(ctx, pg, nowUTC.Add(time.Minute).In(jst))
	afterJST := countRuns(t, pg)
	sc, err := pg.GetSchedule(ctx, "probe-sched")
	require.NoError(t, err)
	t.Logf("UTC-leader fired=%d, JST-takeover fired=%d, last_fired_at=%v — TZ divergence causes skip/dup? utc=%d jst=%d",
		afterUTC, afterJST-afterUTC, sc.LastFiredAt, afterUTC, afterJST-afterUTC)
}

// TestEdgeProbe_BackwardClockStep: last_fired_at ahead of now (NTP step
// back / a fast-clock leader wrote it). How long does scheduling stall?
func TestEdgeProbe_BackwardClockStep(t *testing.T) {
	pg := probeSetup(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	_, err := pg.UpsertSchedule(ctx, "probe-sched", "* * * * *", "probe-job", nil)
	require.NoError(t, err)
	// A leader 10 minutes fast stamped last_fired_at in our future.
	require.NoError(t, pg.UpdateScheduleLastFiredAt(ctx, "probe-sched", now.Add(10*time.Minute)))

	checkAndFireSchedules(ctx, pg, now)
	t.Logf("with future last_fired_at: fired=%d (every-minute schedule silent until wall clock catches up: %v)",
		countRuns(t, pg), countRuns(t, pg) == 0)
}
```

Note for the implementer: `pg.GetSchedule` is in the store interface (`internal/store/store.go:433`). If any signature differs at compile time, adjust the call — keep the logged observations identical.

- [ ] **Step 2: Verify normal builds still exclude the probes**

```bash
go vet ./internal/controller/ && go test ./internal/controller -run TestEdgeProbe -count=1
```

Expected: vet clean; test run reports `no tests to run` / `ok` **without** executing any probe (tag not set).

- [ ] **Step 3: Run the probes**

```bash
go test -tags edgeprobe ./internal/controller -run TestEdgeProbe -count=1 -v
```

Expected: all PASS, each with `t.Logf` lines describing observed behavior. Save the full output (scratchpad) for Task 6.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/edgeprobe_scheduler_test.go
git commit -m "test(edgecase): add W0-2 scheduler clock/TZ boundary probes (edgeprobe tag)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: W0-2 findings + W0 checkpoint

**Files:**
- Modify: `test/edgecase/FINDINGS.md`

**Interfaces:**
- Consumes: Task 5's probe output; Task 4's W0-1 entries.

- [ ] **Step 1: Write FINDINGS entries for W0-2** — one per probe that surfaced surprising behavior (candidates: DST-gap occurrence silently skipped for a whole day; TZ-divergent duplicate or skip; 1-occurrence-per-minute backlog drain; backward-step stall). Confirming-expected observations get a single combined "observations (no violation)" entry — findings files that bury violations under noise are useless.

- [ ] **Step 2: Append a W0 checkpoint section**

```markdown
## Checkpoint: W0 complete
- Scenarios run: W0-1 (pgbouncer), W0-2 (6 scheduler probes)
- Violations: <n> (<ids>)  Observations: <n>
- Impact on later waves: <e.g. "C14 unit-probe pattern reusable for W2
  boundary scenarios; compose overlay pattern validated for W3">
```

- [ ] **Step 3: Commit**

```bash
git add test/edgecase/FINDINGS.md
git commit -m "test(edgecase): record W0-2 probe findings and W0 checkpoint

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

- [ ] **Step 4: Report the checkpoint to the operator** — summarize findings in chat (this is a wave checkpoint, not the final batch report; fixes still wait). The W1 plan gets written after this report.
