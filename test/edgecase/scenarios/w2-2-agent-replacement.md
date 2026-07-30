# W2-2 — agent replacement (same ID re-registers) with orphaned runs

- **Invariants:**
  - **I1 (run accounting)** is the pass/fail limb: after an agent process is
    replaced, no run may be left `Running` with nothing executing it, and no
    run may be failed while something *is* still executing it.
  - **I3 (lock release)** is the second limb: whatever the replacement does to
    a run, the run's `mutex_holders` / `named_lock_slots` rows must be
    released so a successor run can proceed.
- **Stack:** plain `test/ha` compose, **no overlay**. Every compose call is:

  ```bash
  cd test/ha
  docker compose -f docker-compose.ha.yaml up -d --build
  ```

- **Workloads:** `call-parent.payload.json` + `call-child.payload.json`
  (Part A — a linked parent/child pair), `longrun.payload.json` (Part B — a
  300s run so the claim can be caught fresh), `sideeffect.payload.json` +
  `mutex-successor.payload.json` (Parts B2/C — the mutex holder and its
  successor probe).
- **Instrumentation:** none beyond `docker compose logs` and psql. Unlike W2-1
  this scenario does not need Postgres statement logging: every transition it
  cares about is either a controller log line or a `runs`/`step_reports` row.

## Verified API/mechanism (do not re-derive)

Read this before running. Three premises in the W2 plan's Task-3 text need
qualifying, and the runbook is written to test them rather than assume them.

### (1) The startup reconcile is documented, and it is documented as ungraced

`docs/high-availability.md:406-418` is an explicit contract, not an
implementation detail:

> Both agents therefore call `POST /api/v1/agents/{id}/runs/reconcile` on
> startup, after registering and **before the first claim**: the controller
> fails every `Running` run still claimed by that agent ID, with the same
> semantics as the reaper (`Failed`, never re-queued, locks released, `call:`
> descendants cascade-cancelled).

"every `Running` run still claimed by that agent ID" has no time qualifier,
and the code matches: `ListRunningRunIDsByAgent`
(`internal/store/postgres.go:266-285`) is a bare
`SELECT id FROM runs WHERE claimed_by = $1 AND status = 'Running'` (`:271`),
with none of the `claimed_at < NOW() - make_interval(secs => $2)` clause that
its sibling `ListReconcilableRunIDsByAgent` (`:287-309`, predicate at `:294`,
used by the heartbeat path with `heartbeatReconcileGrace = 60s`) carries.
**Get these two the right way round when citing them** — `:294` is the
*graced* query, so citing it for the ungraced one inverts the whole point.
`handleAgentReconcileRuns`
(`internal/controller/api_agent.go:809-831`) calls `failOrphanedRun` per id and
logs `"agent reconcile: failed orphaned run (agent process replaced)"` at
`:828`.

**So "a run claimed one second ago is failed immediately" is the documented
behavior, not a contradiction of it.** Part B must therefore be judged as an
*operational-cost* observation unless it produces something the docs do not
cover. Do not file it as a violation on the strength of the missing grace
alone — that is exactly the mistake W2-1 was corrected for.

### (2) There IS a documented drain contract, and it is "wait forever" by default

The plan's Recording note ("a rolling agent restart kills in-flight work with
no drain window") is **wrong as stated for the product** and must be tested,
not asserted. The agent has a full cordon/drain path:

- `docs/high-availability.md:341` — "Agents also support graceful drain (stop
  claiming = cordon → finish in-progress Runs → exit)."
- `docs/configuration.md:173` — `--drain-timeout duration  Max wait after
  SIGTERM before forced shutdown (0 = wait forever)`. **The default is 0**,
  i.e. the agent waits indefinitely for in-flight runs.
- `internal/agent/shutdown.go:20-36` — the first SIGINT/SIGTERM cancels
  `claimCtx` only and logs `"shutdown signal received; draining in-flight
  runs — press Ctrl-C again to force quit"`; `internal/agent/agent.go:234-248`
  keeps `runCtx` alive (cancelled only after `DrainTimeout`, which is
  unset here), and `agent.go:250-258` deliberately binds the heartbeat to
  `runCtx` so a draining run is not reaped as stuck.

`test/ha/docker-compose.ha.yaml:97-113` passes no `--drain-timeout` to either
agent, so **both agents drain forever by default in this rig**. What actually
truncates the drain is the *orchestrator's* stop grace period. Docker's
*documented* default stop timeout is 10s — but **do not budget on that**: the
2026-07-29 execution measured bare `docker compose restart` sending SIGKILL
**1.013s** after SIGTERM (see the execution notes at the end of this file;
`w2-2/partB-docker-events.txt`). Either way it is a property of the rig's
invocation, not of the product — so a runbook that only ever runs
`docker compose restart agent1` cannot distinguish "the product has no drain"
from "the harness did not wait for it".

**Part B2 exists to make that distinction**, by re-running the same shutdown
with a stop timeout longer than the run. Without it any finding about drain is
unsound.

### (3) The force-shutdown reconcile needs a *second* signal, which no orchestrator sends

`docs/high-availability.md:419-423` documents a best-effort reconcile on
"**force shutdown** (second SIGINT/SIGTERM)". `shutdown.go:26-33` implements
exactly that: it blocks on a second receive from the signal channel before
calling `onForce`. `docker compose restart`, `docker stop` and a Kubernetes
pod deletion all send **one** SIGTERM and then SIGKILL — never a second
SIGTERM. So on every orchestrator-driven restart this path is unreachable and
the orphan window is closed by the *startup* reconcile instead. The docs are
literally accurate ("an operator who skips the drain", i.e. Ctrl-C twice at a
terminal); record the reachability gap as a note, and check during execution
whether `onForce` ever fires.

### (4) Concurrency and topology facts that shape the observation

- `--max-concurrent` defaults to **1** (`docs/configuration.md:169`,
  `cmd/unified-cd-agent/main.go:71-75`; `test/ha` overrides nothing). A
  non-detached parent run occupies its agent's single slot for the whole
  `call:` wait, so **the child is necessarily claimed by the other agent**.
  Confirm this from `runs.claimed_by` rather than assuming it — if both land
  on one agent the reconcile lists both ids and the child is terminalized by
  whichever of the two paths reaches it first.
- The only persisted parent→child edge is `step_reports.child_run_id`
  (`internal/agent/callstep.go:50-66`, `postgres.go:313-328`) and it is
  written by a **separate, error-discarded** HTTP request. **Confirm the link
  exists before injecting** — a cascade test against an unlinked child proves
  nothing (that is W2-5's scenario, not this one).
- `edge-call-parent` runs a **20s `prelude`** before its `call:` step
  (`test/edgecase/workloads/call-parent.payload.json`), so the link cannot
  exist until ~20s after the claim. Budget for it.
- `failOrphanedRun` (`internal/controller/stuckrun_reaper.go:76-90`) is
  `MarkRunStepsInterrupted` → `MarkRunFinished` → `cancelDescendantRuns`, and
  `MarkRunFinished`/`FinishRun` (`postgres.go:746-780`) is a single
  transaction that CASes `status NOT IN (terminal)` **and** deletes
  `mutex_holders` / clears `named_lock_slots` for the run. So I3 release is
  transactional with the run's own terminalization; the non-transactional part
  is only the cascade to descendants.
- `cancelDescendantRuns` marks descendants **`Cancelled`**, not `Failed`
  (`api_runs.go:390-412`). Expect `Failed` parent + `Cancelled` child.

## Baseline (plain stack, before any observation)

BASELINE GATE — confirm all of these before recording anything. If any fails,
STOP and report BLOCKED with the evidence.

```bash
SCRATCH=<scratchpad>/w2-2 ; mkdir -p "$SCRATCH"
cd test/ha
docker compose -f docker-compose.ha.yaml up -d --build

# 1. LB up, all three controllers up, both agents up
curl -s -o /dev/null -w 'readyz=%{http_code}\n' localhost:18080/readyz          # expect 200
docker compose -f docker-compose.ha.yaml ps --format '{{.Service}} {{.State}}'

# 2. Both agents registered and idle
curl -fsS localhost:18080/api/v1/agents -H "Authorization: Bearer ha-admin-token"

# 3. Apply every fixture this scenario uses
for f in call-parent call-child longrun sideeffect mutex-successor; do
  curl -fsS -X POST localhost:18080/api/v1/jobs \
    -H "Authorization: Bearer ha-admin-token" -H 'Content-Type: application/json' \
    --data-binary @../edgecase/workloads/$f.payload.json -o /dev/null -w "$f=%{http_code}\n"
done                                                                  # expect 200 x5

# 4. Lock tables start empty — every later I3 claim is relative to this
psql -c "SELECT (SELECT count(*) FROM mutex_holders) AS mutex,
                (SELECT count(*) FROM named_lock_slots WHERE run_id IS NOT NULL) AS named;"
```

Throughout, `psql` means:

```bash
docker compose -f docker-compose.ha.yaml exec -T postgres psql -U unified -tAc "<sql>"
```

## Part A — clean replacement of the claiming agent, with a linked child

```bash
# A1. Trigger the parent and record the wall clock
date -u +%FT%T.%3NZ | tee "$SCRATCH/partA-timeline.txt"
curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-call-parent"}' \
  | tee "$SCRATCH/partA-parent-trigger.json"
PARENT=<id from the response>
```

```bash
# A2. Wait for the claim; record WHICH agent claimed it — that is the agent to
#     restart, and it is not necessarily agent1.
psql "SELECT id, status, claimed_by, claimed_at FROM runs WHERE id='$PARENT';"
```

```bash
# A3. Wait out the 20s prelude, then poll until the parent→child link exists.
#     THIS IS THE GATE: do not inject until it returns a row.
for i in $(seq 1 40); do
  date -u +%FT%T.%3NZ
  psql "SELECT run_id, step_index, child_run_id FROM step_reports WHERE child_run_id IS NOT NULL;"
  sleep 2
done | tee "$SCRATCH/partA-link-poll.txt"
```

Also record the child's own claim, so the cross-agent topology is on the
record:

```bash
psql "SELECT id, job_name, status, claimed_by, claimed_at FROM runs ORDER BY created_at;" \
  | tee "$SCRATCH/partA-pre-restart-runs.txt"
psql "SELECT (SELECT count(*) FROM mutex_holders) AS mutex,
             (SELECT count(*) FROM named_lock_slots WHERE run_id IS NOT NULL) AS named;" \
  | tee "$SCRATCH/partA-pre-restart-locks.txt"
```

```bash
# A4. Replace the claiming agent.
#     MEASURED, not assumed: bare `restart` here gave the container 1.013s
#     before SIGKILL (NOT the documented 10s) — see the execution notes at the
#     end of this file. This step therefore injects a near-immediate kill.
date -u +%FT%T.%3NZ
docker compose -f docker-compose.ha.yaml restart <claiming-agent>
date -u +%FT%T.%3NZ
```

```bash
# A5. Poll both runs at 1s until both are terminal, with timestamps.
for i in $(seq 1 60); do
  date -u +%FT%T.%3NZ
  psql "SELECT id, job_name, status, claimed_by, updated_at FROM runs ORDER BY created_at;"
  sleep 1
done | tee "$SCRATCH/partA-post-restart-poll.txt"
```

Then capture the attribution evidence:

```bash
docker compose -f docker-compose.ha.yaml logs controller1 controller2 controller3 \
  | grep -E "agent reconcile|cascade cancel|stuck-run reaper" | tee "$SCRATCH/partA-controller-reconcile.txt"
docker compose -f docker-compose.ha.yaml logs <claiming-agent> | tee "$SCRATCH/partA-agent.log"
psql "SELECT run_id, step_index, status, child_run_id FROM step_reports ORDER BY run_id, step_index;" \
  | tee "$SCRATCH/partA-step-reports.txt"
```

Record, with real timestamps: restart issued → SIGTERM observed in the agent
log → process exit → `"agent registered"` → the reconcile log line → parent
`Failed` → child `Cancelled`. **The number that matters is restart-issued →
child terminal**, because that is the operator-visible blast radius.

**I1 check:** no run may still be `Running` at the end of the poll. **I3
check:** `mutex_holders` and `named_lock_slots` must be back to the baseline
counts (both zero here — `edge-call-parent`/`edge-call-child` declare no
mutex, so Part A's I3 limb is a null result and Part C is where I3 is really
tested; say so rather than claiming Part A proved lock release).

## Part B — the no-grace consequence (restart within 5s of the claim)

```bash
# B1. Trigger a fresh long run and catch the claim as tightly as possible.
curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-longrun"}'
# poll at 0.5s until claimed_at is non-null, printing the wall clock each time
for i in $(seq 1 120); do
  date -u +%FT%T.%3NZ
  psql "SELECT id, status, claimed_by, claimed_at, NOW()-claimed_at AS claim_age FROM runs WHERE job_name='edge-longrun' ORDER BY created_at DESC LIMIT 1;"
  sleep 0.5
done | tee "$SCRATCH/partB-claim-poll.txt"
```

```bash
# B2. The instant claimed_at is non-null, restart the claiming agent.
date -u +%FT%T.%3NZ ; docker compose -f docker-compose.ha.yaml restart <claiming-agent> ; date -u +%FT%T.%3NZ
```

```bash
# B3. Poll to terminal, then compute the age of the claim at the moment it was failed.
for i in $(seq 1 60); do
  date -u +%FT%T.%3NZ
  psql "SELECT id, status, claimed_at, updated_at, updated_at-claimed_at AS age_at_terminal FROM runs WHERE job_name='edge-longrun' ORDER BY created_at DESC LIMIT 1;"
  sleep 1
done | tee "$SCRATCH/partB-poll.txt"
```

`age_at_terminal` is the headline number: compare it against the heartbeat
path's `heartbeatReconcileGrace = 60s` (`api_agent.go:71`) and the stuck-run
reaper's `grace = 60s` (`main.go:404`). If it is well under 60s, the two
orphan definitions demonstrably disagree, and that disagreement — not the
failure itself — is what the entry records.

Also capture how the failure is **reported to the operator**, which is the
part the docs do not cover:

```bash
curl -fsS "localhost:18080/api/v1/runs/$RUN" -H "Authorization: Bearer ha-admin-token" \
  | tee "$SCRATCH/partB-run.json"
curl -fsS "localhost:18080/api/v1/runs/$RUN/logs" -H "Authorization: Bearer ha-admin-token" \
  | tee "$SCRATCH/partB-run-logs.txt"
psql "SELECT step_index, name, status, started_at, ended_at FROM step_reports WHERE run_id='$RUN' ORDER BY step_index;" \
  | tee "$SCRATCH/partB-steps.txt"
```

Specifically ask: does anything in the run's own record say *why* it failed
(an appended system log line, an error field, a step message), or must the
operator correlate against a controller log line they may not have? The
queued-run reaper appends a system log line for its victims
(`queuedrun_reaper.go:70`); check whether this path does anything equivalent.

## Part B2 — the drain contrast (does the documented drain actually hold?)

Part B alone cannot distinguish a product with no drain from a harness that
did not wait for one (mechanism note 2). Repeat the shutdown with a stop grace
period longer than the run.

```bash
# B2a. A run that finishes in ~120s, on a known agent.
curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-sideeffect"}'
psql "SELECT id, status, claimed_by, claimed_at FROM runs WHERE job_name='edge-sideeffect' ORDER BY created_at DESC LIMIT 1;"

# B2b. Stop with a grace period that exceeds the remaining work.
date -u +%FT%T.%3NZ
docker compose -f docker-compose.ha.yaml stop -t 200 <claiming-agent>
date -u +%FT%T.%3NZ
```

Expected if the documented drain holds: the agent logs `"shutdown signal
received; draining in-flight runs"`, keeps heartbeating (so the stuck-run
reaper never touches it), the run reaches `Succeeded`, `"agent deregistered"`
follows, and the container exits **before** the 200s deadline. Then:

```bash
docker compose -f docker-compose.ha.yaml start <claiming-agent>
docker compose -f docker-compose.ha.yaml logs controller1 controller2 controller3 \
  | grep "agent reconcile" | tee "$SCRATCH/partB2-controller-reconcile.txt"
```

The reconcile on the way back up must find **nothing** — a drained agent has
no `Running` runs left under its ID. Capture the agent log and the run's final
status:

```bash
docker compose -f docker-compose.ha.yaml logs <claiming-agent> | tee "$SCRATCH/partB2-agent.log"
psql "SELECT id, job_name, status, updated_at FROM runs WHERE job_name='edge-sideeffect' ORDER BY created_at;" \
  | tee "$SCRATCH/partB2-runs.txt"
```

**If the drain holds here, Part B's failure is squarely attributable to the
stop grace the harness gave it — measured at 1.013s for bare `docker compose
restart`, not the documented 10s (execution notes below) — and the finding is
about *defaults* (a sub-second-to-10s docker grace and a 30s k8s
`terminationGracePeriodSeconds` both truncate a drain the product documents as
unbounded), not about a missing mechanism.** If the drain
does **not** hold — the run is failed or the process exits early despite the
200s budget — that contradicts `docs/high-availability.md:341` and
`docs/configuration.md:173` and **is** a violation. Record whichever happened,
with the agent log as evidence.

## Part C — I3: lock release and successor acquisition

Part A's runs hold no mutex, so I3 needs its own arm using the mutex fixtures.

```bash
# C1. edge-sideeffect declares `spec.concurrency.mutex: edge-mutex`.
curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-sideeffect"}'
#     The column is `mutex_name`, NOT `name` (001_init.up.sql:123) — see the
#     execution note below; a typo here lost the first Part B2 lock reading.
psql "SELECT run_id, mutex_name FROM mutex_holders;" | tee "$SCRATCH/partC-held.txt"   # expect one row
```

```bash
# C2. Kill the holder's agent process mid-run, then let it come back and reconcile.
date -u +%FT%T.%3NZ
docker compose -f docker-compose.ha.yaml restart <claiming-agent>
for i in $(seq 1 60); do
  date -u +%FT%T.%3NZ
  psql "SELECT (SELECT count(*) FROM mutex_holders) AS mutex,
               (SELECT count(*) FROM named_lock_slots WHERE run_id IS NOT NULL) AS named,
               (SELECT status FROM runs WHERE job_name='edge-sideeffect' ORDER BY created_at DESC LIMIT 1) AS run;"
  sleep 1
done | tee "$SCRATCH/partC-release-poll.txt"
```

```bash
# C3. The successor must actually acquire it — an empty table is necessary, not sufficient.
curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-mutex-successor"}'
for i in $(seq 1 30); do
  date -u +%FT%T.%3NZ
  psql "SELECT id, job_name, status FROM runs WHERE job_name='edge-mutex-successor';"
  sleep 2
done | tee "$SCRATCH/partC-successor-poll.txt"
curl -fsS "localhost:18080/api/v1/runs/$SUCCESSOR/logs" -H "Authorization: Bearer ha-admin-token" \
  | tee "$SCRATCH/partC-successor-logs.txt"     # expect `acquired-mutex-ok`
```

A successor that reaches `Succeeded` with `acquired-mutex-ok` in its log is
the I3 pass. A successor stuck `Pending` with an empty `mutex_holders` would
be a *different* defect (the Pending→Queued transition, W2-9's territory) —
distinguish them before recording.

## Recording (severity guidance)

- **A run left `Running` with no process executing it = major (I1).** This is
  the pass/fail. Evidence must be a `runs` row plus the absence of the owning
  agent process, not an inference.
- **A descendant left `Running`/`Queued` after its parent is terminalized =
  major (I1/I3).** Only recordable if the parent→child link was confirmed
  present before injection (mechanism note 4) — otherwise it is W2-5, not
  this scenario.
- **Immediate failure of a just-claimed run = observation, not violation**,
  because `docs/high-availability.md:414-415` promises exactly "every
  `Running` run still claimed by that agent ID" with no time qualifier
  (mechanism note 1). Record the measured claim age at failure and the
  contrast with the 60s heartbeat/reaper graces, and state the operational
  cost. Escalate only if something *undocumented* is found alongside it —
  e.g. no operator-visible reason on the run itself, or a run failed that a
  live process was still executing.
- **A drain that does not hold with an adequate grace period = major**
  (contradicts `docs/high-availability.md:341` and
  `docs/configuration.md:173`). A drain that holds, but is truncated by the
  orchestrator's default stop grace, = **observation** about defaults — with
  the measured numbers, and explicitly labelled as a deployment-default issue
  rather than a code defect.
- **A mutex/named-lock row surviving its run's terminalization = major (I3).**
  `FinishRun` does the release in the same transaction as the status CAS
  (`postgres.go:746-780`), so a survivor would mean the run never reached a
  terminal status at all — check that before concluding.
- Timing that lands within one expected interval (the stop grace — **measured
  at 1.013s for bare `docker compose restart`, not the documented 10s**, see
  the execution notes below; 15s heartbeat; 30s reaper tick) = observation with
  the measured number.

## Execution notes (added after the 2026-07-29 run — read before re-running)

- **`docker compose restart <svc>` gives the container 1.013 s, not 10 s.**
  Measured with `docker events` (`w2-2/partB-docker-events.txt`): `kill sig=15`
  at epoch `1785345889.394` (`:7`), `kill sig=9` at `1785345890.407` (`:11`),
  `die exit=137` (`:13`). **The cause of the ~1s is not established** — no
  `stop_grace_period` is set in `test/ha/docker-compose.ha.yaml` and no further
  instrumentation was run; the measurement stands regardless.
  So **a bare `restart` cannot test the drain** — it truncates it before the
  agent can finish anything. Any later scenario that means "graceful restart"
  must pass `-t <seconds>` explicitly. This invalidated the plan's premise that
  a rolling restart has "no drain window": the window exists, the harness was
  not giving it to the agent.
- **The drain itself works, unbounded, exactly as documented.** With
  `stop -t 200` the agent held an in-flight run for **107.31 s** past SIGTERM,
  the run reached `Succeeded`, `"agent deregistered"` was logged, and the
  container exited 0 (`w2-2/partB2-drain.txt`). Use this arm as the control
  whenever a scenario claims an agent shutdown lost work.
- **`--max-concurrent` defaults to 1, and that is load-bearing here.** The
  parent's `call:` step occupies its agent's only normal slot for the whole
  child wait, so the child is claimed by the *other* agent and Part A is a
  genuine cross-agent cascade. It also means the detached pool (16 slots) is
  the only thing still polling on the draining agent, which is why the
  shutdown log is 16 `"claim" context canceled` ERROR lines — noise, not a
  fault.
- **Budget ~20 minutes** of wall clock for all four arms plus the two controls,
  dominated by Part A's 20 s prelude + link wait and Part B2's 107 s drain.
  `up -d --build` was cached.
- **The `call:` link appears immediately, not after a delay.** `callstep.go:62`
  sends the `ChildRunID` report right after creating the child, so the link was
  present at the very first poll sample. The 20 s prelude is what you wait for,
  not the link.
- **Two no-fault controls are worth keeping.** Cancelling a `call:` parent
  through the public API, and cancelling an ordinary single-step run, both
  reproduce the dangling-`Running`-step row with no fault injection at all —
  which is what turned a Part A curiosity into a finding with a system-wide
  blast radius. Run them before concluding that any step-row anomaly is
  specific to agent replacement.
- **`mutex_holders`'s columns are `mutex_name, run_id, acquired_at`** (not
  `name`), and `named_lock_slots` is `pool_name, slot_id, run_id, acquired_at`.
  A `\d` beats guessing; the first Part B2 capture lost its lock reading to a
  column-name typo.
- **`docker events --since/--until` over a past window returned nothing here.**
  Start the capture *before* the injection if the exit signal and code matter.

## Teardown

```bash
docker compose -f docker-compose.ha.yaml down -v
docker compose -f docker-compose.ha.yaml ps -a
```
