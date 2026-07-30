# W2-4 — queued-run reaper grace boundary during a full agent outage

- **Invariants:**
  - **I1 (run accounting)** is the pass/fail limb: a run must not be failed
    while an eligible agent is live and claiming, and must not stay
    non-terminal forever when nothing can ever run it.
  - **I5 (bounded recovery)** is the timing limb: the interval between a run
    becoming reapable and the reaper acting on it must be bounded by the
    reaper's sweep interval, and the operator must be able to tell why.
- **Stack:** `test/ha` + `oneway.override.yaml` (for the `/data` bind mount the
  mutex workloads write to) + `queuedgrace.override.yaml` (this scenario's
  overlay, `UNIFIED_QUEUED_RUN_GRACE=30s`). Every compose call is:

  ```bash
  cd test/ha
  export COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml -f ../edgecase/compose/queuedgrace.override.yaml"
  docker compose $COMPOSE_FILES up -d --build
  ```

- **Workloads:** `tick.payload.json` (`edge-tick`, Parts A and B),
  `mutex-hog.payload.json` + `sideeffect.payload.json` (both hold `edge-mutex`
  — Part C), `podcap-job.payload.json` (`edge-podcap-job`, Part D).
- **Instrumentation:** psql sampling at 0.2 s for every boundary; Postgres
  statement logging (W2-1's technique, `log_statement='all'` +
  `log_line_prefix='%m [%p] h=%h '`) for **Part B only**, reverted at teardown.

Throughout, `psql` means:

```bash
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "<sql>"
```

## Verified mechanism (read before running; do not re-derive)

### (1) The predicate, and what actually flips it

`ListUnclaimableQueuedRuns` (`internal/store/postgres.go:1275-1286`), called as
`RunQueuedRunReaper(ctx, st, 30s, queuedRunGraceDefault(), 90s)`
(`cmd/controller/main.go:410`) so `$1 = minAge = grace` (30 s under this
scenario's overlay, 5 m by default) and `$2 = staleAfter = 90s`:

```sql
SELECT r.id, r.agent_selector
FROM runs r
WHERE r.status = 'Queued'
  AND r.created_at < NOW() - make_interval(secs => $1)     -- minAge = grace
  AND NOT EXISTS (
    SELECT 1 FROM agents a
    WHERE a.last_seen_at >= NOW() - make_interval(secs => $2)   -- staleAfter = 90s
      AND (r.agent_selector = '{}' OR r.agent_selector <@ a.labels)
  )
```

Three consequences that shape every arm:

- **The age clock is `created_at`, not a queued-at stamp.** There is no
  `queued_at` column — `runs` carries only `created_at` and `updated_at`
  (`internal/store/migrations/001_init.up.sql:213-226`), and the Pending→Queued
  transition writes `updated_at = NOW()` (`postgres.go:657-660`), which the
  reaper never reads. **That `updated_at` write is the only observable stamp
  for "became Queued" and Part C depends on catching it by polling.**
- **What flips the `NOT EXISTS` back off is agent *registration*, not
  claiming.** Once a label-matching row exists with a fresh `last_seen_at` the
  run is invisible to the reaper even though it is still `Queued` and not yet
  claimed. So Part B's boundary is the moment the `agents` row appears, and
  that is the timestamp to measure — not the claim.
- **A cleanly stopped agent vanishes from the predicate immediately, not after
  90 s.** W2-1 established that a healthy agent's SIGTERM path calls
  `Deregister` → `DELETE FROM agents WHERE id = $1`
  (`internal/agent/agent.go:343-350`, `postgres.go:1149-1152`), measured at
  **0.9 s** after `docker compose stop`. `staleAfter` only matters when the row
  survives — i.e. after a `kill-hard`, which is why Part C uses one.

### (2) Budget latency at the nominal 30 s, load at `interval / N`

W2-1 measured **2.15-2.40** advisory-lock acquisitions per nominal 30 s
interval on this 3-replica rig and the W2 plan drew the consequence that the
effective sweep cadence is `interval / N` ≈ 13.3 s. **W2-3 disproved the
latency half of that**: instrumenting the sweeps themselves (not the lock
acquisitions) showed 8 clusters / 17 executions over 210 s with a
**within-cluster spread of 0.002-0.011 s** and a **between-cluster gap of
29.987-30.001 s** (`w2-3/armD0-sweeps.txt`), and the four reap latencies it
measured (19.246 / 28.061 / 11.283 / 8.013 s) are uniform over 30 s with three
of the four above 13.3 s.

**Rule for this scenario: budget query load at `interval / N`; budget
worst-case reap latency at the nominal `interval` = 30 s, and say which you are
budgeting.** Concretely, a run becomes reapable at `created_at + 30 s` and is
failed at the first sweep after that, i.e. anywhere in
`[created_at + 30 s, created_at + 60 s)`. **The reap instant is therefore not
under the experimenter's control, and Part B must report a distribution across
repeated trials rather than a single outcome per offset.**
Full reasoning: `docs/superpowers/plans/2026-07-30-edge-case-campaign-w2.md:47`
and `test/edgecase/scenarios/w2-3-reaper-boundary.md` §(2).

### (3) The sweep phase is measurable, and Part B is uninterpretable without it

If the agent returns and the run survives, that is only evidence the reaper
*lost* if the reaper actually had an opportunity. Enable statement logging for
Part B and extract every execution of the `ListUnclaimableQueuedRuns` SELECT;
each execution is a sweep instant. A trial then reads as three timestamps —
run `created_at`, `agents` row insert, nearest sweep with run age > 30 s — and
the winner is whichever of the last two came first. Reap instants observed in
Parts A and C give an independent check on the phase (all sweeps are ≡ mod
30 s while the controllers are not restarted).

### (4) Part D is a documented-intent check, not a defect hunt

`postgres.go:1264-1274` carries an 11-line comment ending **"Do not add a
capability clause to this query"**: the omission is deliberate, because
capabilities only make claiming *stricter*, so a label-unclaimable run is also
capability-unclaimable, while a run that is label-claimable but
capability-unschedulable is **intentionally left Queued** and surfaced through
the JobDetail unschedulable banner instead. `RunQueuedRunReaper`'s own doc
comment (`queuedrun_reaper.go:18-28`) repeats it. **Confirm the behavior and
record it as an observation.** The open question worth answering is what else,
besides that banner, tells an operator — check `GET /api/v1/runs/{id}`, the run
log, `/metrics`, and the controller logs, not just the WebUI.

**The fixture has to require `pod`, not `container` — corrected during
execution.** `agentCapabilities` (`internal/agent/agent.go:137-139`) reports
`["native"]` only when no container runtime is detected, and the `test/ha`
agents report **`["native","container"]`** (`w2-4/baseline-gate.txt`), so a
merely non-native job is perfectly schedulable here. The fixture is therefore
`podcap-job.payload.json` (`edge-podcap-job`): a `podTemplate` carrying a
pod-level key other than `containers` (`nodeSelector`) makes
`PodTemplateNeedsKubernetes` true (`internal/dsl/podtemplate.go:40-46`), so
`dsl.RequiredCaps` yields **`pod`** (`internal/dsl/capabilities.go:24-33`) —
which no standard agent can ever advertise. Crucially there is **no blanket
`kubernetes` label pin any more** (`internal/controller/api_runs.go:70-83`
documents its removal), so the run keeps the author's
`agentSelector: [kind:linux]` and is genuinely label-claimable.
**Verify the agents' advertised capabilities live from `GET /api/v1/agents`
before concluding anything.**

### (5) What the docs promise (search these before filing a violation)

- `docs/high-availability.md:89` — "Only the leader fails Queued runs that no
  live agent can claim within `UNIFIED_QUEUED_RUN_GRACE`".
- `docs/high-availability.md:352-356` — during a full agent outage "queued runs
  are failed once they have **waited** longer than the queued-run reaper grace
  — configurable on the controller via `UNIFIED_QUEUED_RUN_GRACE` (default
  `5m`). Raise it if such outages can exceed the default."
- `docs/troubleshooting.md:32-73` — "Run stays `Queued` forever": the
  documented cause is "no connected agent satisfies the job's `agentSelector`,
  or every agent that does is already at its concurrency limit", and the
  documented remedy is to cancel the run manually.
- `docs/troubleshooting.md:74-130` — the unschedulable banner, and
  `GET /api/v1/jobs/{name}/schedulability` as the API behind it.

`ListUnclaimableQueuedRuns`' own doc comment is an **unexported helper's
comment and is therefore not a documented contract** under this campaign's
classification rule (`FINDINGS.md:476-514`, the W1 checkpoint). It establishes
*intent*, which is why Part D is an observation, not a violation.

## Baseline gate

Confirm all of these before recording anything. If any fails, STOP and report
BLOCKED with the evidence.

```bash
SCRATCH=<scratchpad>/w2-4 ; mkdir -p "$SCRATCH"
cd test/ha
export COMPOSE_FILES="-f docker-compose.ha.yaml -f ../edgecase/compose/oneway.override.yaml -f ../edgecase/compose/queuedgrace.override.yaml"
docker compose $COMPOSE_FILES up -d --build

curl -s -o /dev/null -w 'readyz=%{http_code}\n' localhost:18080/readyz
docker compose $COMPOSE_FILES ps --format '{{.Service}} {{.State}}'
curl -fsS localhost:18080/api/v1/agents -H "Authorization: Bearer ha-admin-token"

for f in tick mutex-hog sideeffect podcap-job; do
  curl -fsS -X POST localhost:18080/api/v1/jobs \
    -H "Authorization: Bearer ha-admin-token" -H 'Content-Type: application/json' \
    --data-binary @../edgecase/workloads/$f.payload.json -o /dev/null -w "$f=%{http_code}\n"
done                                                            # expect 200 x4
```

Four gates specific to this scenario:

```bash
# G1. The overlay took. There is NO positive confirmation line — the check is
#     the ABSENCE of the WARN, which only fires on a malformed value.
docker compose $COMPOSE_FILES logs controller1 controller2 controller3 \
  | grep -c "invalid UNIFIED_QUEUED_RUN_GRACE"        # expect 0
docker compose $COMPOSE_FILES exec -T controller1 printenv UNIFIED_QUEUED_RUN_GRACE
docker compose $COMPOSE_FILES exec -T controller2 printenv UNIFIED_QUEUED_RUN_GRACE
docker compose $COMPOSE_FILES exec -T controller3 printenv UNIFIED_QUEUED_RUN_GRACE

# G2. Host <-> DB clock skew, because every arm schedules host-side sleeps
#     against DB-column deadlines. Record it; correct for it if non-trivial.
psql "SELECT NOW();" ; date -u +%FT%T.%3NZ

# G3. Read the agents' ADVERTISED capabilities — Part D's premise (mechanism
#     note 4). Expect `["native","container"]`, NOT `native` only: the test/ha
#     agents detect a container runtime, so a merely non-native job is
#     schedulable here and the fixture has to require `pod`. An earlier draft of
#     this gate expected `native` only; treating that as the pass condition
#     stops the scenario on a correct stack.
curl -fsS localhost:18080/api/v1/agents -H "Authorization: Bearer ha-admin-token" \
  | tee "$SCRATCH/agents-caps.json"
# PASS = no agent advertises `pod`. FAIL (STOP) = any agent advertises `pod`,
# because then `edge-podcap-job` is claimable and Part D has no premise.

# G4. Scheduler leader, from the only leadership log line there is (NOT readyz).
docker compose $COMPOSE_FILES logs controller1 controller2 controller3 \
  | grep "scheduler became leader"
```

```bash
docker compose $COMPOSE_FILES logs controller1 | head -40 | tee "$SCRATCH/controller-boot.txt"
```

## Standard sampler

One shape for every arm, so columns are comparable. `NOW()` is the DB clock and
every derived boundary is computed from DB columns, never the host clock. Run
the loop **inside** the postgres container so per-sample overhead is a local
psql invocation (~10-20 ms) rather than a `docker exec` round trip.

```bash
sample_loop() {  # $1 = run id, $2 = iterations, $3 = sleep seconds
  docker compose $COMPOSE_FILES exec -T postgres sh -c "
    for i in \$(seq 1 $2); do
      psql -U unified -tAc \"SELECT NOW(), r.id, r.status, r.created_at, r.updated_at,
             r.claimed_by, r.claimed_at,
             EXTRACT(EPOCH FROM (NOW() - r.created_at)) AS age,
             (SELECT count(*) FROM agents) AS agent_rows,
             (SELECT max(last_seen_at) FROM agents) AS newest_hb
             FROM runs r WHERE r.id = '$1';\"
      sleep $3
    done"
}
```

## Part A — the reap, and the operator-facing reason line

```bash
# A1. Take both agents away cleanly. Per mechanism note 1 this DELETEs both
#     rows within ~1s, so the NOT EXISTS conjunct is satisfied immediately —
#     staleAfter never enters into it.
date -u +%FT%T.%3NZ | tee "$SCRATCH/armA-stop.txt"
docker compose $COMPOSE_FILES stop agent1 agent2 | tee -a "$SCRATCH/armA-stop.txt"
psql "SELECT NOW(), id, last_seen_at FROM agents;" | tee -a "$SCRATCH/armA-stop.txt"   # expect no rows

# A2. Trigger. The scheduler still queues it — queueing consults no agent.
curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-tick"}' \
  | tee "$SCRATCH/armA-trigger.json"
RUN=<id>

# A3. Sample at 0.2s from before the Pending->Queued transition through the reap.
sample_loop "$RUN" 600 0.2 | tee "$SCRATCH/armA-poll.txt"
```

```bash
# A4. The reason line. This is the arm's payload: queuedrun_reaper.go:70 appends
#     a system log row at step index -1 BEFORE failing the run, with the
#     agentSelector spelled out. Confirm it reaches the read API.
curl -fsS "localhost:18080/api/v1/runs/$RUN/logs" -H "Authorization: Bearer ha-admin-token" \
  | tee "$SCRATCH/armA-run-logs.json"
psql "SELECT step_index, stream, ts, line FROM logs WHERE run_id='$RUN' ORDER BY ts;" \
  | tee "$SCRATCH/armA-logs-db.txt"
docker compose $COMPOSE_FILES logs controller1 controller2 controller3 \
  | grep -i "queued-run reaper" | tee "$SCRATCH/armA-reaper-log.txt"
```

**Numbers to compute, all from DB columns in `armA-poll.txt`:**

- `created_at`; the first sample showing `Queued` (Pending→Queued latency);
  `runs.updated_at` at the reap.
- `reap_latency = updated_at - (created_at + 30 s)`. **Compare against the
  nominal 30 s** (mechanism note 2). Anything inside it is an observation.
- Whether the reason line's text carries the `(requires agent labels: ...)`
  suffix — `edge-tick` declares `agentSelector: [kind:linux]`, so it must.
- Whether `docs/troubleshooting.md:32-73`'s "stays `Queued` forever" claim
  survives contact with this arm. It should not.

## Part B — the race at the edge (the point of the scenario)

Both agents stay stopped between trials. Each trial:

1. Trigger `edge-tick`; read `created_at` and the DB clock in the same psql
   call so the deadline is computed on the DB clock.
2. Sleep host-side until `created_at + 30 s + offset − lead`, where `lead` is
   the measured `docker compose start` → `agents`-row-insert latency (calibrate
   it once, in trial 0, and re-check it every trial from the capture).
3. `docker compose start agent1`.
4. Sample at 0.2 s until the run is terminal or `Running`.
5. `docker compose stop agent1` (clean stop ⇒ row deleted ⇒ next trial starts
   from a true no-agent state; confirm `SELECT count(*) FROM agents` = 0).

Offsets: **grace−5 s, grace−1 s, grace+1 s**, each run **at least twice** (three
times if the budget allows). Statement logging is on for the whole of Part B.

```bash
psql "ALTER SYSTEM SET log_statement='all';"
psql "ALTER SYSTEM SET log_line_prefix='%m [%p] h=%h ';"
psql "SELECT pg_reload_conf();"
```

**Per trial, record — all measured, none intended:**

| Quantity | Source |
|---|---|
| `created_at` | `runs` |
| intended offset | the trial's parameter |
| `t_reg` = first `agents` row insert | 0.2 s sampler, `agent_rows` 0→1 |
| `t_reg − (created_at + 30 s)` | **the measured offset**, which is what the outcome must be keyed to |
| `t_sweep` = first `ListUnclaimableQueuedRuns` execution with run age > 30 s | Postgres statement log |
| outcome | `Running`/`Succeeded` (agent won) or `Failed` (reaper won) |
| `t_claim − t_reg` | how much of the window the agent needs after registering |

After the last trial:

```bash
docker compose $COMPOSE_FILES logs postgres > "$SCRATCH/partB-postgres-full.log"
grep -n "FROM runs r" "$SCRATCH/partB-postgres-full.log" | grep -i "Queued" \
  | tee "$SCRATCH/partB-sweeps.txt"
gzip -9 "$SCRATCH/partB-postgres-full.log"
psql "ALTER SYSTEM RESET log_statement;"
psql "ALTER SYSTEM RESET log_line_prefix;"
psql "SELECT pg_reload_conf();"
```

**Recording:** a run **failed while an eligible agent was already live and
claiming** is a **major (I1)** — the evidence has to be the `agents` row
present (and, better, a claim poll in the statement log) *before*
`runs.updated_at`. A run that survives a `grace+1 s` return is **not** a null
result: it quantifies how much of the nominal 30 s post-grace window is
survivable, which is the number an operator planning a maintenance restart
actually needs. Report the distribution, not one outcome per offset.

## Part B5 — amplify the reaper's stale-list window with a backlog (added during execution; this is the arm that found the major)

Parts B1-B3 as the plan writes them cannot produce the major, and that is a
result about the *axis*, not about the system: the reaper re-evaluates the
`NOT EXISTS` conjunct in the same SELECT that lists the run, so an agent that
registers **before** a sweep is always seen. The only way the reaper can fail a
run with a live eligible agent is if the agent appears **after** the SELECT and
**before** the reaper's own UPDATE reaches that run. That distance is 4 ms for a
single-run batch — unhittable with container-level injection, the same wall
W2-3's Arm D1 hit.

**But it is not a fixed 4 ms.** `runQueuedRunReaperOnce`
(`internal/controller/queuedrun_reaper.go:59-79`) lists the whole reapable set
once and then loops `AppendLog` → `MarkRunFinished` → `cancelDescendantRuns` per
run, re-reading nothing. The window is therefore `N × per-run cost`, and a full
agent outage — the documented trigger — *is* a backlog. So:

```bash
# Both agents cleanly stopped (agents rows deleted). Then:
sh <scratch>/partB-backlog.sh 250 bk1
```

which submits 250 `edge-tick` runs via `tools/bulk-submit.sh`, computes the
first sweep instant strictly after the earliest run's `created_at + grace` from
the measured grid, and issues a raw `docker start` so registration lands ~0.25 s
into that sweep's loop. **Detector, stable and readable at leisure** (the run
stays `Failed` and nothing re-drives it):

```bash
psql "SELECT r.id, r.status, r.claimed_by, r.claimed_at, r.updated_at
      FROM runs r WHERE r.claimed_by IS NOT NULL AND r.status = 'Failed';"
```

A row here is unambiguous: only an agent writes `claimed_by`, and only the
reaper writes the step-index −1 reason line, so a run carrying both was claimed
by a live agent and then failed as unclaimable. Confirm the pairing with
`SELECT ... FROM logs WHERE step_index = -1`.

## Part C — the `created_at` clock: grace consumed while Pending

The age gate is `created_at`, so a run that spends the grace period legitimately
blocked (waiting on a mutex, or on git resolution) enters `Queued` **already
past `minAge`** and is reapable on the very next sweep. This arm produces that
state with **no DB mutation** — every transition is the product's own.

```bash
# C1. Both agents up. The hog claims edge-mutex.
docker compose $COMPOSE_FILES start agent1 agent2
curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-mutex-hog"}' | tee "$SCRATCH/armC-hog.json"
psql "SELECT * FROM mutex_holders;" | tee "$SCRATCH/armC-mutex.txt"

# C2. The victim: same mutex, so it stays Pending (tryQueueRun rolls back on
#     the mutex_holders unique violation, postgres.go:546-555).
curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-sideeffect"}' | tee "$SCRATCH/armC-victim.json"
VICTIM=<id>
# confirm it is Pending and let it age well past minAge (30s)
```

```bash
# C3. Remove both agents WITHOUT a clean stop, so the rows survive and age out
#     via staleAfter instead of vanishing (the drain would otherwise wait for
#     the 600s hog — W2-2 measured an unbounded drain).
sh ../edgecase/tools/inject.sh kill-hard agent1
sh ../edgecase/tools/inject.sh kill-hard agent2
date -u +%FT%T.%3NZ | tee "$SCRATCH/armC-kill.txt"
```

The chain that follows is entirely the product's:

`stuck-run reaper` fails the hog at `max(claimed_at+60 s, last_seen_at+90 s)`
+ sweep → `MarkRunFinished` releases `mutex_holders` in the same transaction
(`postgres.go:746-780`) → the scheduler's 200 ms loop queues the victim →
the victim is `Queued` with `age ≫ 30 s` and both `agents` rows now stale past
`staleAfter` → the **next** queued-reaper sweep fails it.

```bash
# C4. Sample the victim at 0.2s across the whole chain. The Queued->Failed
#     window is the number this arm exists to produce, and it is only
#     observable by catching updated_at at the Pending->Queued write.
sample_loop "$VICTIM" 2400 0.2 | tee "$SCRATCH/armC-poll.txt"
psql "SELECT id, job_name, status, created_at, updated_at FROM runs ORDER BY created_at;" \
  | tee "$SCRATCH/armC-runs.txt"
curl -fsS "localhost:18080/api/v1/runs/$VICTIM/logs" -H "Authorization: Bearer ha-admin-token" \
  | tee "$SCRATCH/armC-victim-logs.json"
```

**Record:** `created_at`, the sample at which the status first reads `Queued`
(and the `updated_at` written by that transition), the reap `updated_at`, and
**`queued_window = reap_updated_at − queued_updated_at`**. Also record the run's
total age at reap, to make the point that the 30 s grace bought it nothing.

**Severity rule (from the plan):** major if the run is failed before one full
claim poll could have occurred, else observation with the measured window. The
claim long-poll on this rig is 30 s (`ClaimDetached(..., "30s", ...)`,
`internal/agent/agent.go:411-415`), so compare `queued_window` against 30 s and
say which side it falls on. Note the confound honestly: in this arm no agent
was alive to poll at all, so "could not have been claimed" is true for a second
reason; the transferable claim is about the **clock**, not about this run's
chances.

## Part D — the deliberate capability gap (documented-intent check)

Both agents up and healthy for the whole arm.

```bash
docker compose $COMPOSE_FILES start agent1 agent2   # confirm 2 rows in agents
curl -fsS localhost:18080/api/v1/jobs/edge-podcap-job/schedulability \
  -H "Authorization: Bearer ha-admin-token" | tee "$SCRATCH/armD-schedulability.json"
curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-podcap-job"}' \
  | tee "$SCRATCH/armD-trigger.json"
DRUN=<id>
sample_loop "$DRUN" 60 5 | tee "$SCRATCH/armD-poll.txt"     # 5 minutes = 10x grace
```

**Confirm:** `status = Queued` at every sample; `age` climbing past `10 × minAge`
with no reap; the queued-reaper log grep still empty for this run id.

**Then answer the interesting question — what tells an operator?** Check each
surface and record present/absent:

```bash
curl -fsS "localhost:18080/api/v1/runs/$DRUN" -H "Authorization: Bearer ha-admin-token"
curl -fsS "localhost:18080/api/v1/runs/$DRUN/logs" -H "Authorization: Bearer ha-admin-token"
curl -fsS "localhost:18080/api/v1/jobs/edge-podcap-job/schedulability" -H "Authorization: Bearer ha-admin-token"
curl -fsS localhost:18080/metrics | grep -E "unifiedcd_runs_current|unifiedcd_agents"
docker compose $COMPOSE_FILES logs controller1 controller2 controller3 | grep -i "$DRUN"
```

`GET /api/v1/jobs/{name}/schedulability` (`api_jobs.go:194-217`,
`schedulability.go:24-49`) is the one surface that names the cause. Record
whether it is **job-scoped rather than run-scoped** — i.e. whether an operator
holding a run id can reach it without already knowing the diagnosis — and
whether `/metrics` distinguishes this from an ordinary backlog
(`internal/metrics/collector.go:56-59` exports only
`unifiedcd_runs_current{status}` counts, no age).

Also record the **scope limit of the exemption**: it is conditional on a
label-matching live agent existing. Stop both agents and the same run becomes
reapable like any other, because the `NOT EXISTS` conjunct no longer cares why
nothing can claim it. Demonstrate that at the end of the arm — it is one
`stop` plus one sweep, and it converts "left Queued forever on purpose" into
"left Queued forever only while an agent that cannot run it is connected".

## Recording (severity guidance)

- **A run failed while an eligible agent was live and claiming = major (I1)**
  (Part B). The evidence must be the `agents` row present before
  `runs.updated_at`, ideally with the agent's claim poll in the statement log.
- **Part C**: major if failed before one full claim poll could have occurred,
  else observation with the measured window. The **transferable** finding is
  that the grace clock runs from `created_at`, so time spent in a healthy,
  intended wait (mutex, git resolution) is charged against the outage budget;
  judge that against `docs/high-availability.md:352-356`'s wording ("once they
  have **waited** longer than the ... grace") and say plainly whether it is a
  contradiction or a wording gap.
- **Part D = observation** (documented intent, `postgres.go:1264-1274`). What is
  worth filing alongside it is the **diagnosability** question, and only if the
  surfaces really are as thin as the code suggests — verify, do not assume.
- **Part A's reason line, if it works, is a positive finding.** The campaign
  has filed two entries about reaps that leave no operator-visible reason
  (`FINDINGS.md:652`, W2-2); this reaper appending its reason to the run's own
  log at step index −1 is the counterexample and should be recorded as such
  rather than passed over because nothing broke.
- `docs/troubleshooting.md:32-73` telling an operator the run "stays `Queued`
  forever" and to cancel it manually, when the reaper will fail it after the
  grace, is a **docs gap (minor)** if confirmed — check that the section really
  does describe the reaper's own target case before filing.
- Do not re-file W2-3's `a.id IS NULL` major. It has a second, unexercised
  limb noted at `FINDINGS.md:710`: `ListUnclaimableQueuedRuns` also decides
  liveness by `EXISTS (SELECT 1 FROM agents ...)`, so a live agent whose row
  was deleted is invisible here too. **If this scenario happens to exercise
  that limb, cross-reference the existing entry — do not open a new major.**

## Teardown

```bash
# ALTER SYSTEM cannot run inside a transaction block, so these must be separate
# psql invocations (W2-3 execution note).
psql "ALTER SYSTEM RESET log_statement;"
psql "ALTER SYSTEM RESET log_line_prefix;"
psql "SELECT pg_reload_conf();"
sh ../edgecase/tools/inject.sh nginx-unblock || true   # no service argument
docker compose $COMPOSE_FILES down -v
docker compose $COMPOSE_FILES ps -a
```

## Execution notes (added after the 2026-07-29 run — read before re-running)

- **The Part D fixture in the first draft of this runbook was wrong twice, and
  the baseline gate caught both.** `container-job.payload.json` (a non-`native`
  job) returned **400** — `invalid yaml: line 10: field image not found in type
  dsl.StepEntry`; there is no step-level `image:` key. And the premise was
  wrong anyway: **the `test/ha` agents advertise `["native","container"]`**, not
  `["native"]` (`w2-4/baseline-gate.txt`), because a container runtime is
  present in the agent image, so a merely non-native job is schedulable here.
  The fixture is now `podcap-job.payload.json`, whose pod-level `nodeSelector`
  forces `RequiredCaps` → `pod` (`internal/dsl/podtemplate.go:40-46`). Fixed in
  commit `5080981`. **Generalisation for later waves: `agentCapabilities`
  (`internal/agent/agent.go:137-139`) is runtime-detected, so any scenario that
  depends on a capability being *absent* must read `GET /api/v1/agents` first —
  `pod` is the only capability a standard agent can never have.**
- **The plan's Part B axis is the wrong one, and this is the scenario's
  methodological result.** Walking the return across `grace ± n` does not walk
  it across anything the system reacts to: the reaper's only opportunities are
  the discrete sweep instants, so the outcome is decided by
  `registration − (first sweep past created_at + grace)`. Trials at −5, −1, +1,
  **+15 and +25** all survived, because those triggers happened to land where
  the boundary fell 25-27 s before the next sweep. **To make the boundary a real
  coin flip you must phase-lock the trigger** (`partB-phase.sh`): the sweep grid
  sits at a stable `(epoch mod interval)`, so choosing `created_at mod 30` fixes
  the head-room. At `created_at mod 30 ≈ 26.6` (≈2.4 s of head-room) a **+5.086 s**
  return lost and a **+1.110 s** return won. Report offsets measured from the
  `agents`-row appearance, never intended offsets.
- **The sweep grid drifts, slowly.** `22:26:59.016` → `22:31:59.148` is 10 ticks
  in 300.132 s, i.e. **30.013 s per tick**, so a phase computed once is good for
  tens of minutes but not for hours; re-measure if the run is long. Grid position
  here: wall seconds `:29.0x`/`:59.0x` early in the session, `:29.8`/`:59.8` two
  hours later.
- **`docker compose start <agent>` is 4× slower than `docker start` and much
  noisier**, because compose re-resolves `agent1`'s `depends_on:
  agent-enroll: service_completed_successfully` and re-runs the one-shot
  enroll container first: **1.697 s** to the `agents`-row insert versus
  **0.400 s** for `docker start unified-cd-ha-agent1-1`
  (`w2-4/partB-calibration.txt`, `w2-4/partB-calibration2.txt`). Use the raw
  form for any timed return. (Harmless side effect either way: the agent logs
  `enrollment token rejected (expired or already consumed); continuing with the
  existing credential` on every restart.)
- **Three traps in dumping the Postgres statement log**, all of which cost time
  here:
  1. `docker logs c 2>&1 > file` writes an **empty** file — Postgres logs to
     stderr, and `2>&1` before the redirect points stderr at the *old* stdout.
     Write `docker logs c > file 2>&1`.
  2. The reaper's SQL is a multi-line Go const, so the `%m ... LOG:  execute
     stmtcache_...:` prefix line ends **before** the SQL. Grep with `-B1`:
     `grep -B1 "SELECT r.id, r.agent_selector" | grep LOG:`.
  3. `docker compose logs --since/--until` still returns nothing for a past
     window (the same quirk W2-2 and W2-3 hit). Dump `--tail N` and filter by
     timestamp. Cost here: 400k lines ≈ 36 MB for ~1 h on a mostly idle
     3-replica stack; 613 KB gzipped.
- **A long reaper loop really does exclude the other replicas.** Every sweep
  cluster in `w2-4/partB-sweeps.txt` holds 2-3 executions (98 in 38 clusters =
  **2.58 per nominal interval**, independently reproducing W2-3's clustering
  result on a different query) — except `22:43:59`, which holds **one**, because
  the 475 ms backlog loop kept the advisory lock long enough for the other two
  replicas' `pg_try_advisory_lock` to fail. So the "no leader stickiness"
  behaviour W2-1 measured is a property of the holds being ~1 ms, not of the
  lock.
- **Part C needs no DB mutation, and the trick is worth reusing.** To get a run
  that is `Queued` while already past `minAge`, let a mutex holder be reaped
  rather than cancelling it: `FinishRun` releases `mutex_holders` inside the
  same transaction that terminalizes the run (`postgres.go:746-780`), so
  `kill-hard` on the agent → stuck-run reap at `last_seen_at + 90 s` → mutex
  released → scheduler queues the blocked run **196.9 ms** later, with every
  transition performed by the product. Do **not** use `docker compose stop` on
  the agent holding the hog: the drain is effectively unbounded (W2-2 measured
  107.3 s) and would wait out the 600 s sleep.
- **`docker compose stop` on an idle agent removes it from the reaper's
  predicate in ~1-2 s, not 90 s.** Measured: rows present at `19:42:03.319816`,
  `stop` returned `19:42:04.771`, `count(*) = 0` at `19:42:05.06929`
  (`w2-4/armA-stop.txt`). The `staleAfter = 90s` term in
  `ListUnclaimableQueuedRuns` therefore never engages on a clean stop — it only
  matters after a `kill-hard`, which is what Part C uses. This is the flip side
  of W2-1's deregistration fact and it makes Parts A and B *faster* than the
  plan assumed, not slower.
- **Budget ~35 minutes of wall time** for a full pass: baseline gate ~2 min,
  Part D ~7 min (a 6.4-minute poll, deliberately longer than the 5 m *default*
  grace so a failed override cannot be mistaken for the documented behaviour),
  Part A ~2 min, seven Part B trials ~110 s each ≈ 13 min, Part B5 ~3 min,
  Part C ~6 min.
