# W2-3 — stuck-run reaper boundary timing

- **Invariants:**
  - **I1 (run accounting)** is the pass/fail limb: a run must not be failed
    while a live process is still executing it, and must not stay `Running`
    when nothing is.
  - **I3 (lock release)** is checked as a side condition on every reap.
  - **I5 (bounded recovery)** is the timing limb: reap latency past
    eligibility must be bounded by the reaper's effective sweep cadence.
- **Stack:** `test/ha` + `../edgecase/compose/oneway.override.yaml` (needed for
  `inject.sh nginx-block`). Every compose call is:

  ```bash
  cd test/ha
  export COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml"
  docker compose $COMPOSE_FILES up -d --build
  ```

- **Workloads:** `longrun.payload.json` (300s run — Arms A/B/C),
  `call-parent.payload.json` + `call-child.payload.json` (Arm D — a linked
  parent/child pair).
- **Instrumentation:** psql sampling for every boundary; Postgres statement
  logging (W2-1's technique) for Arm D only, reverted at teardown.

Throughout, `psql` means:

```bash
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "<sql>"
```

## Verified mechanism (read before running; do not re-derive)

### (1) The predicate, and what each conjunct actually gates

`ListStuckRunIDs` (`internal/store/postgres.go:1238-1248`), called as
`RunStuckRunReaper(ctx, st, 30s, 90s, 60s)` (`cmd/controller/main.go:404`) so
`$1 = staleAfter = 90s` and `$2 = grace = 60s`:

```sql
SELECT r.id FROM runs r
LEFT JOIN agents a ON r.claimed_by = a.id
WHERE r.status = 'Running'
  AND r.claimed_at IS NOT NULL
  AND r.claimed_at  < NOW() - make_interval(secs => $2)     -- grace  = 60s
  AND (a.id IS NULL OR a.last_seen_at < NOW() - make_interval(secs => $1))  -- stale = 90s
```

Eligibility is therefore `max(claimed_at + 60s, last_seen_at + 90s)` in the
**heartbeat-loss** branch, and **`claimed_at + 60s` alone** in the
**`a.id IS NULL`** (agent-row-gone) branch. The second branch has **no time
component of its own** — that is the whole point of Arm C.

### (2) Budget from the effective cadence, not the nominal interval

W2-1 established that the stuck-run reaper's advisory lock is a **per-tick**
mutex with no stickiness: every replica ticks every 30s, acquires, sweeps, and
releases ~1ms later, so on N replicas the sweep runs up to N times per nominal
interval. Measured on this exact 3-replica rig: **2.15 acquisitions per 30s
nominal interval → a ~13.3s effective sweep cadence**
(`w2-1/lock-analysis-idle.txt`). **Budget reap latency against ~13.3s, not
30s.** A latency near 30s on this rig is *worth explaining*, not expected.

### (3) Arm B as the plan writes it is not runnable, and that is a result

The plan says to "block the agent immediately after the claim so
`last_seen_at + 90s` fires well before `claimed_at + 60s`". That ordering is
unreachable. The heartbeat interval is 15s (`internal/agent/heartbeat.go:10`),
and the claim itself refreshes `last_seen_at` (`handleAgentClaim` calls
`UpsertAgentOnClaim`, `api_agent.go:150`, whose `ON CONFLICT DO UPDATE` sets
`last_seen_at = NOW()`, `postgres.go:1129-1139`). So at the instant of a claim
`last_seen_at ≈ claimed_at`, and thereafter `last_seen_at ≥ claimed_at - 15s`
for as long as the agent lives. Hence

    last_seen_at + 90s  ≥  claimed_at + 75s  >  claimed_at + 60s

**always**, and the 60s grace conjunct can never be the binding one in the
heartbeat branch. Arm B is therefore rewritten in two halves: **B1** measures
`last_seen_at - claimed_at` to make the inequality an observation rather than
an assertion, and **B2** forces the inverted ordering by backdating
`last_seen_at` directly in psql. **B2 mutates DB state rather than injecting a
fault** — it demonstrates the code path, it is not a naturally occurring
observation, and must be labelled that way in any finding (same convention the
W2 plan applies to W2-6 Arm C).

### (4) `docker compose stop` cannot age an agent out — use `nginx-block`

W2-1 measured a healthy agent's SIGTERM path deregistering itself in **0.9s**
(`internal/agent/agent.go:343-350` builds a fresh 5s context after ctx cancel
and calls `Deregister`, i.e. `DELETE FROM agents WHERE id = $1`), so `stop`
never produces a stale `last_seen_at`. Bare `docker compose restart` is also
not graceful — W2-2 measured **1.013s** to SIGKILL, not the documented 10s.
This scenario therefore uses **`nginx-block`** everywhere it needs staleness,
and `kill-hard` only for controllers.

### (5) The three ways the `agents` row disappears, and the one path that does not restore it

All three execute the same statement shape:

| Path | Statement | Trigger |
|---|---|---|
| `DeleteStaleAgents` (`postgres.go:1225-1233`) | `DELETE FROM agents WHERE last_seen_at < NOW() - $1::interval` | 1m tick, 5m threshold, **no leader election**, on every replica (`main.go:382-398`) |
| `DeleteAgent` (`postgres.go:1149-1152`) | `DELETE FROM agents WHERE id = $1` | `DELETE /api/v1/agents/{agentId}` → `handleAgentDeregister` (`api_agents.go:48-55`), agent-auth bound to its own ID (`server.go:246`, `registerAgentIdentityRoutes` applies `agentAuth` + `requireAgentPathIdentity`) |
| the agent's own shutdown | the same endpoint, via `Client.Deregister` | `agent.go:343-350` |

Restoration is **asymmetric**, and this is the load-bearing fact for Arm C:

- **Heartbeat does NOT restore the row.** `handleAgentHeartbeat` calls
  `TouchAgent` (`api_agent.go:95`), which is a bare
  `UPDATE agents SET last_seen_at = NOW() WHERE id = $1`
  (`postgres.go:1145-1148`). With the row gone this updates **zero rows**,
  returns no error, and the handler still returns **204**. The agent logs
  nothing (`internal/agent/heartbeat.go:47-51` only warns on transport error).
- **Claim polling DOES restore it.** `handleAgentClaim` calls
  `UpsertAgentOnClaim` (`api_agent.go:150`) on **every** claim poll, and that
  is an `INSERT ... ON CONFLICT (id) DO UPDATE` (`postgres.go:1129-1139`) —
  it re-inserts a deleted row and sets `last_seen_at = NOW()`.

So the row's absence persists exactly as long as the agent is **not polling
for claims**: either because it is partitioned (nginx-block), or because every
claim slot is busy. Note that on this rig the **detached** claim pool keeps
polling even while the single normal slot (`--max-concurrent` defaults to 1,
`docs/configuration.md:169`) is occupied, so an unblocked busy agent should
still restore its row within one detached long-poll (≤60s,
`maxClaimTimeout` at `api_agent.go:129`). **Measure that restore latency in
Arm C2 rather than assuming either outcome** — it is what decides whether the
reaper or the agent wins.

### (6) `failOrphanedRun` is three non-transactional writes

`stuckrun_reaper.go:76-90`: `MarkRunStepsInterrupted` (`:81`) →
`MarkRunFinished` (`:84`) → `cancelDescendantRuns` (`:88`). No transaction
spans them. `ListStuckRunIDs` selects only `status = 'Running'`, so once
`:84` commits the run is never re-listed and a crash before `:88` is **never
re-driven by anything**. `cancelDescendantRuns` (`api_runs.go:390-412`) also
logs-and-continues on error, so a transient failure has the same effect as a
crash. That is Arm D's target. Note the two facts W2-2 established that bound
what Arm D can claim: `cancelDescendantRuns` has **four** direct call sites
(`api_runs.go:382`, `api_agent.go:626`, `queuedrun_reaper.go:77`,
`stuckrun_reaper.go:88`) and seven entry points once `failOrphanedRun`'s
callers are expanded; and `MarkRunStepsInterrupted` inside `failOrphanedRun`
is scoped to its own `runID` only, never to the descendants it cancels.

### (7) Topology facts that shape the observation

- `--max-concurrent` defaults to **1**, so a parent's `call:` step occupies its
  agent's only normal slot for the whole child wait and **the child is claimed
  by the other agent**. Confirm from `runs.claimed_by`; do not assume.
- The only persisted parent→child edge is `step_reports.child_run_id`
  (`callstep.go:50-66`, `postgres.go:313-328`), written by a separate
  error-discarded HTTP request. **Arm D must gate on the link existing** — a
  cascade test against an unlinked child proves nothing (that is W2-5).
- `edge-call-parent` runs a **20s `prelude`** before its `call:` step, so
  budget for it.
- `MarkRunFinished`/`FinishRun` (`postgres.go:746-780`) is a single transaction
  that CASes the status **and** releases `mutex_holders` / `named_lock_slots`,
  so I3 release is transactional with the run's own terminalization.
  `edge-longrun` and the `call:` pair declare no mutex, so I3 here is a null
  result — say so rather than claiming it was tested.

## Baseline gate

Confirm all of these before recording anything. If any fails, STOP and report
BLOCKED with the evidence.

```bash
SCRATCH=<scratchpad>/w2-3 ; mkdir -p "$SCRATCH"
cd test/ha
export COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml"
docker compose $COMPOSE_FILES up -d --build

curl -s -o /dev/null -w 'readyz=%{http_code}\n' localhost:18080/readyz
docker compose $COMPOSE_FILES ps --format '{{.Service}} {{.State}}'
curl -fsS localhost:18080/api/v1/agents -H "Authorization: Bearer ha-admin-token"

for f in longrun call-parent call-child; do
  curl -fsS -X POST localhost:18080/api/v1/jobs \
    -H "Authorization: Bearer ha-admin-token" -H 'Content-Type: application/json' \
    --data-binary @../edgecase/workloads/$f.payload.json -o /dev/null -w "$f=%{http_code}\n"
done                                                            # expect 200 x3
```

Two extra gates specific to this scenario:

```bash
# G1. Does Postgres actually accept the interval DeleteStaleAgents passes?
#     Go renders 5*time.Minute as the string "5m0s" and the query is
#     `NOW() - $1::interval` (postgres.go:1226-1228). If this errors, the
#     stale-agent cleanup has never run at all and that is the finding.
psql "SELECT '5m0s'::interval;"

# G2. Identify the scheduler leader from the only leadership log line there is
#     — NOT from readyz (W1: the readyz gap is rig-side and unexplained).
docker compose $COMPOSE_FILES logs controller1 controller2 controller3 \
  | grep "scheduler became leader"
```

Record the boot log once so later "job not wired" ambiguity is pre-answered
(W2-1's rule: a key with no holder is three different results):

```bash
docker compose $COMPOSE_FILES logs controller1 | head -40 | tee "$SCRATCH/controller-boot.txt"
```

## Standard sampler

Every arm uses the same sample shape, so the columns are comparable across
arms. `NOW()` is the DB clock; every derived boundary is computed from DB
columns, never from the host clock.

```bash
sample() {  # $1 = run id
  psql "SELECT NOW(),
               r.id, r.status, r.claimed_by, r.claimed_at, r.updated_at,
               a.id AS agent_row, a.last_seen_at,
               EXTRACT(EPOCH FROM (NOW() - r.claimed_at))    AS claim_age,
               EXTRACT(EPOCH FROM (NOW() - a.last_seen_at))  AS hb_age
        FROM runs r LEFT JOIN agents a ON r.claimed_by = a.id
        WHERE r.id = '$1';"
}
```

## Arm A — the 90s staleness boundary (natural, no DB mutation)

```bash
# A1. Trigger and catch the claim tightly.
curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-longrun"}' \
  | tee "$SCRATCH/armA-trigger.json"
RUN=<id>
for i in $(seq 1 120); do sample "$RUN"; sleep 0.5; done | tee "$SCRATCH/armA-claim-poll.txt"
# stop as soon as claimed_at is non-null; record claimed_by -> AGENT
```

```bash
# A2. One-way partition of the claiming agent.
date -u +%FT%T.%3NZ | tee "$SCRATCH/armA-block.txt"
sh ../edgecase/tools/inject.sh nginx-block "$AGENT" | tee -a "$SCRATCH/armA-block.txt"
```

```bash
# A3. Sample every 5s until terminal, then 5 minutes further (Arm C0 rides on
#     this same block: the agent row must survive to last_seen_at + 300s).
for i in $(seq 1 90); do sample "$RUN"; sleep 5; done | tee "$SCRATCH/armA-poll.txt"
```

```bash
# A4. Attribution.
docker compose $COMPOSE_FILES logs controller1 controller2 controller3 \
  | grep -E "stuck-run reaper|deleteStaleAgents" | tee "$SCRATCH/armA-reaper-log.txt"
psql "SELECT run_id, step_index, status FROM step_reports WHERE run_id='$RUN' ORDER BY step_index;" \
  | tee "$SCRATCH/armA-steps.txt"
```

**Numbers to compute, all from DB columns in `armA-poll.txt`:**

- `hb_freeze` = the last `last_seen_at` observed (the agent's final heartbeat).
- `eligible_at` = `max(claimed_at + 60s, hb_freeze + 90s)` — and record which
  term won, that is Arm B1's evidence.
- `reap_latency` = `runs.updated_at - eligible_at`. **Compare against the
  ~13.3s effective cadence, not 30s.** A latency > 30s needs an explanation.
- `last_seen_at - claimed_at` at the moment of the claim (mechanism note 3).

**I1 check:** the agent process is still executing the step at the moment the
run is failed (it is partitioned, not dead). Record that as the zombie
interval — but note this restates W1-5's already-filed unfenced-execution
finding; **cross-reference it, do not re-file**. What is new here is the
*timing*, not the zombie.

## Arm C0 — `DeleteStaleAgents` fires (rides on Arm A's block)

Keep the agent blocked. `DeleteStaleAgents` compares against `last_seen_at`, so
the deletion is due at `hb_freeze + 300s`, on a 1-minute tick, from any of the
three replicas.

```bash
for i in $(seq 1 40); do
  psql "SELECT NOW(), count(*) FROM agents WHERE id='$AGENT';"
  sleep 10
done | tee "$SCRATCH/armC0-agentrow-poll.txt"
docker compose $COMPOSE_FILES logs controller1 controller2 controller3 \
  | grep deleteStaleAgents | tee "$SCRATCH/armC0-delete-log.txt"
```

Record: `hb_freeze`, the last sample with the row present, the first sample
with it absent, and the `deleteStaleAgents deleted=N` line's timestamp. Note
explicitly that the two windows are **strictly nested** (90s ≪ 300s), so the
run was already terminal for ~210s by the time the row went away — i.e. in
normal operation the `a.id IS NULL` branch is **never** the branch that reaps
anything. Then unblock:

```bash
sh ../edgecase/tools/inject.sh nginx-unblock
```

## Arm B — which conjunct binds

### B1 — the natural ordering (measurement, no mutation)

Already captured by Arm A: report `last_seen_at - claimed_at` at claim time and
show `hb_freeze + 90s > claimed_at + 60s`. State the general bound from
mechanism note 3 and label it **derived**, since one measurement does not prove
a universal.

### B2 — force the inverted ordering (DB mutation, labelled)

```bash
# B2a. Fresh run, catch the claim, partition immediately.
RUN2=<id from a fresh edge-longrun trigger>
# poll at 0.5s until claimed_at is non-null
sh ../edgecase/tools/inject.sh nginx-block "$AGENT2"

# B2b. Backdate the heartbeat past the 90s staleness window while the run is
#      still inside its 60s claim grace. THIS IS A DIRECT DB MUTATION — it is
#      a demonstration of the code path, not a naturally occurring state.
psql "UPDATE agents SET last_seen_at = NOW() - interval '95 seconds' WHERE id='$AGENT2';"
date -u +%FT%T.%3NZ

# B2c. Sample every 2s through claimed_at+60s and past it.
for i in $(seq 1 90); do sample "$RUN2"; sleep 2; done | tee "$SCRATCH/armB2-poll.txt"
```

**Expected if the grace conjunct binds:** the run stays `Running` while
`claim_age < 60` even though `hb_age > 90`, and is failed on the first sweep
after `claim_age` crosses 60. **Record `runs.updated_at - (claimed_at + 60s)`**
— that is the same reap-latency quantity as Arm A, measured against the other
conjunct, and the two should agree if the sweep cadence is the only variable.
If the run is failed before `claim_age` reaches 60, the grace is not being
applied and that is a violation of the predicate as written.

Unblock afterwards.

## Arm C — the `a.id IS NULL` branch (the interesting arm)

C0 above showed the branch's precondition arises naturally. C1 and C2 measure
what the branch actually does. Both delete the row with
`DELETE FROM agents WHERE id = ...` — **the identical statement**
`DeleteStaleAgents` (`postgres.go:1226`) and `DeleteAgent` (`postgres.go:1150`)
execute — so the code path is the product's, while the *precondition* is
induced directly. Label it that way.

### C1 — partitioned agent, row deleted inside the 90s window

```bash
RUN3=<fresh edge-longrun>   # catch the claim, record claimed_at
sh ../edgecase/tools/inject.sh nginx-block "$AGENT3"
psql "DELETE FROM agents WHERE id='$AGENT3';" ; date -u +%FT%T.%3NZ
for i in $(seq 1 90); do sample "$RUN3"; sleep 2; done | tee "$SCRATCH/armC1-poll.txt"
```

**The question:** is the run failed at `claimed_at + 60s` (+ sweep latency),
i.e. **~30s earlier** than Arm A's `last_seen_at + 90s`? Report both numbers
side by side. Also confirm the row stays absent for the whole poll (a blocked
agent cannot reach `handleAgentClaim`, so nothing re-inserts it) — that is the
concrete form of "reapable with no upper time bound".

### C2 — healthy agent, row deleted (the sharp variant)

**No partition.** The agent is alive, executing, and heartbeating every 15s.

```bash
RUN4=<fresh edge-longrun>   # catch the claim
# wait until claim_age is ~55s so eligibility is imminent and the race is tight
psql "DELETE FROM agents WHERE id='$AGENT4';" ; date -u +%FT%T.%3NZ
for i in $(seq 1 180); do sample "$RUN4"; sleep 1; done | tee "$SCRATCH/armC2-poll.txt"
docker compose $COMPOSE_FILES logs "$AGENT4" | tee "$SCRATCH/armC2-agent.log"
curl -fsS "localhost:18080/api/v1/runs/$RUN4/logs" -H "Authorization: Bearer ha-admin-token" \
  | tail -40 | tee "$SCRATCH/armC2-run-logs.txt"
```

Four things to record, in this order:

1. **Whether the run is failed while the agent is demonstrably alive.** The
   `tick N` log lines are the liveness proof — if they continue past
   `runs.updated_at`, a healthy heartbeating agent had its run failed. That is
   an **I1 violation** and it is materially different from W1-5's partitioned
   zombie: there is no partition and no fault on the agent side.
2. **Whether the heartbeat restores the row.** It must not (mechanism note 5);
   `agent_row` should stay NULL across heartbeat instants. Confirm the agent
   logs no `"agent heartbeat failed"` — a silent 204 over a zero-row UPDATE is
   the diagnosability half of the finding.
3. **When the row comes back**, i.e. the first sample with `agent_row` non-NULL
   after the delete. Attribute it to the detached claim pool
   (`UpsertAgentOnClaim`), and report the measured latency. This bounds the
   exposure window on *this* rig and is what makes the difference between
   "reapable forever" and "reapable for ≤N seconds" — do not claim the former
   if the measurement shows the latter.
4. **Whether the reaper or the restore wins**, with the numbers.

### C3 — is the branch bounded in the other direction?

From C1's state (row absent, agent still blocked), confirm across a further
~3 minutes of sampling that eligibility, once reached, never expires: there is
no upper bound in the predicate. This is a **negative** observation about the
SQL, best stated as such rather than dressed up as a measurement.

## Arm D — `failOrphanedRun`'s non-transactional writes

### D0 — measure the window before trying to hit it

Do this **first**. An attempt count is only interpretable next to a window
width. Use W2-1's statement-logging technique:

```bash
psql "ALTER SYSTEM SET log_statement='all';
      ALTER SYSTEM SET log_line_prefix='%m [%p] h=%h ';
      SELECT pg_reload_conf();"
```

Then drive **one natural reap of a linked parent**: trigger `edge-call-parent`,
wait out the 20s prelude, gate on the link existing, block the parent's agent,
and let the reaper fire. Afterwards extract from the Postgres log, for the
parent's run id, the timestamps of:

- `MarkRunStepsInterrupted`'s UPDATE (`stuckrun_reaper.go:81`),
- `MarkRunFinished`/`FinishRun`'s transaction (`:84`, `postgres.go:746-780`),
- `ListChildRunIDs`' SELECT (`:88` → `postgres.go:313-328`).

```bash
docker compose $COMPOSE_FILES exec -T postgres sh -c 'cat /var/lib/postgresql/data/log/*.log' \
  > "$SCRATCH/armD-postgres.log"
grep -n "$PARENT" "$SCRATCH/armD-postgres.log" | tee "$SCRATCH/armD-window.txt"
```

**Δ = (first `ListChildRunIDs` statement) − (`FinishRun` COMMIT)** is the
window width. Report it in milliseconds and derive the per-attempt hit
probability against the kill granularity actually used. **Label the
probability derived.**

Gate on the link first, always:

```bash
psql "SELECT run_id, step_index, child_run_id FROM step_reports WHERE child_run_id IS NOT NULL;"
```

### D1 — 10 timed attempts (capped; report the count either way)

Per attempt:

1. Trigger `edge-call-parent`; wait for the claim and for the link row.
2. Confirm the child is `Running` and claimed by the *other* agent.
3. `nginx-block` the parent's agent, then `DELETE FROM agents WHERE id=<parent
   agent>` so eligibility collapses to `claimed_at + 60s` (Arm C1's
   accelerated reap) — this makes the reap instant predictable to within one
   ~13.3s sweep instead of one 30s+90s wait.
4. Across that sweep window, `kill-hard` all three controllers on a tight
   rolling loop (a few hundred ms apart), then bring them back with
   `docker compose $COMPOSE_FILES start controller1 controller2 controller3`.
5. **Detector:** parent `Failed` **and** child not `Cancelled`. Because
   `ListStuckRunIDs` only selects `Running`, nothing re-drives the cascade, so
   the state is stable and can be read at leisure.

```bash
psql "SELECT id, job_name, status, updated_at FROM runs ORDER BY created_at;" \
  | tee -a "$SCRATCH/armD1-attempts.txt"
```

Run a **no-kill control** first and confirm the child *is* `Cancelled`, so a
miss is distinguishable from a broken detector. Cap at **10** attempts and
**report the attempt count whether or not the window is hit**. If it is not
hit, file the code-read argument with its `file:line` evidence, the measured
window from D0, and an explicit **"not reproduced live"** label. Do not claim
the window is unreachable.

## Recording (severity guidance)

- **A run failed while a live, heartbeating, unpartitioned agent is executing
  it = major (I1).** This is Arm C2's payload if it reproduces. Evidence must
  be the run's own log lines continuing past `runs.updated_at`, not an
  inference from the agent being "up".
- **A descendant left uncancelled after its parent is reaped = major (I1).**
  Only recordable if the link was confirmed present before injection.
- **The `a.id IS NULL` branch's missing time component:** judge against the
  documented contract. `ListStuckRunIDs`' own doc comment
  (`postgres.go:1235-1237`) is an **unexported-helper comment and therefore not
  a documented contract** under this campaign's classification rule. Search
  `docs/high-availability.md`, `docs/troubleshooting.md` and
  `docs/configuration.md` for the reaper's windows **before** filing —
  W2-1 filed two "obvious defects" that turned out to be blessed at
  `docs/high-availability.md:91-92`. If the docs describe the 90s window as
  *the* liveness gate and say nothing about the deleted-row branch, that is a
  documentation gap at minimum and a violation if the observed behaviour
  contradicts what they promise.
- **Boundary latency within one effective sweep (~13.3s) = observation** with
  the measured number. Latency beyond one *nominal* interval (30s) = worth its
  own explanation.
- **Heartbeat silently failing to restore a deleted row = minor
  (diagnosability, I5)** on its own; it becomes part of the major above only
  when it is what keeps a healthy agent's run reapable.
- Restating W1-5's unfenced-execution zombie = **cross-reference, do not
  re-file**.

## Teardown

```bash
psql "ALTER SYSTEM RESET log_statement; ALTER SYSTEM RESET log_line_prefix; SELECT pg_reload_conf();"
sh ../edgecase/tools/inject.sh nginx-unblock || true
docker compose $COMPOSE_FILES down -v
docker compose $COMPOSE_FILES ps -a
```
