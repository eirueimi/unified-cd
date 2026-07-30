# W2-7 — two live agent processes sharing one agent ID

- **Invariants** (quoted verbatim from
  `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:44-55`):
  - **I1 (run accounting)** — "every API-accepted run reaches exactly one
    terminal state; no phantom runs from duplicate fires/webhooks" (`:48`). This
    is the primary invariant, and the fit needs stating precisely rather than
    loosely: a run that is failed while its executor is still executing it
    **does** reach exactly one terminal state, so the literal first clause of I1
    is not what breaks. What breaks is the run-accounting property the clause
    exists to protect — the terminal state recorded is *false*, and it was
    written by a reconcile path that had no evidence the work stopped. Parts A,
    B and D each have to argue this on their own facts; do not assert a blanket
    I1 violation across all three.
  - **I3 (no lock leaks)** — "mutex/semaphore/concurrency slots are released
    when the holder reaches a terminal state (verified by a successor run
    acquiring the lock AND by direct inspection of `mutex_holders` /
    `named_lock_slots`)" (`:50`). **In scope here, and in an unusual direction:**
    when a mutex-holding run is reaped out from under a *live* executor, the
    lock is released while the holder is still holding it in the real world. I3
    as written is satisfied (the slot is released) — so the finding, if any, is
    the *opposite* of a leak, and must be filed as such rather than as an I3
    violation. W2-5's entry had to be corrected for mislabelling I3; do not
    repeat that by calling a premature release a "leak".
  - **I6 (zombie containment)** — "after the controller fails a run, observed
    agent-side behavior is *measured and documented* (not pass/fail: the
    architecture has no hard fencing; the operator judges acceptability)"
    (`:53`). This is the invariant the zombie limb of Parts A/B/D reports
    against, and by its own wording it produces a measurement, **not** a
    violation.
  - **I2 (at-most-once side effects)** — "step side effects execute at most once
    (detected via an append-only side-effect log on a shared volume, closing the
    gap `ha_test.go` documents: upserted step reports cannot reveal
    re-execution)" (`:49`). Cited only where `edge-sideeffect` is used, and only
    for what the shared `/data/sideeffect.log` actually shows. Two processes
    writing to one file is the instrument I2 asks for; a *zombie* continuing to
    write after its own run was failed is a single execution continuing, not a
    second execution, and must not be dressed up as an I2 violation.
  - **NOT I5.** `docs/high-availability.md`'s recovery bounds are about returning
    to steady state after a fault; nothing here injects an outage the system is
    expected to recover from. If a reap latency is measured, report it as a
    number, not against an I5 bound.
- **Stack:** `test/ha` plus **two** overlays —
  `../edgecase/compose/oneway.override.yaml` (the shared `/data` bind mount for
  `edge-sideeffect`, plus nginx's runtime-writable blocklist as a fallback tool)
  and `../edgecase/compose/dupagent.override.yaml` (this scenario's overlay; adds
  `agent1b`). Every compose call is:

  ```bash
  cd test/ha
  export COMPOSE_FILES="-f docker-compose.ha.yaml \
    -f ../edgecase/compose/oneway.override.yaml \
    -f ../edgecase/compose/dupagent.override.yaml"
  export MSYS_NO_PATHCONV=1          # Git Bash rewrites container paths (W2-5)
  docker compose $COMPOSE_FILES up -d --build
  ```

  `agent1b` carries `profiles: ["dup"]`, so that `up -d` does **not** start it.
  Start it with `docker compose $COMPOSE_FILES up -d --build agent1b`, stop it
  with `docker compose $COMPOSE_FILES stop agent1b`. `docker compose ps` will not
  list it unless `--profile dup` is passed — pass it in every census.

  Throughout, `psql` means:

  ```bash
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "<sql>"
  ```

- **`agent2` is stopped for the whole scenario, deliberately.** Both `test/ha`
  agents carry `kind:linux`, so with `agent2` up any `edge-*` run may be claimed
  by it and every arm below becomes a coin toss. `docker compose stop agent2`
  before the baseline gate (it deregisters itself cleanly — W2-1) and record that
  the agent inventory is therefore expected to hold exactly one row.
- **Workloads:** `longrun.payload.json` (`edge-longrun`, 300 × `tick N` at ~1/s),
  `sideeffect.payload.json` (`edge-sideeffect`, mutex `edge-mutex`, 120 appends
  to the shared `/data/sideeffect.log`), `mutex-successor.payload.json`
  (`edge-mutex-successor`, same mutex, one echo). Applied with
  `POST /api/v1/jobs`; triggered with `POST /api/v1/runs`, body
  `{"jobName":"..."}`.
- **Clear `test/edgecase/sideeffect-data/` before starting** — W1-5 and W2-5 both
  left `sideeffect.log` behind and a stale file makes every append count wrong.
  Record the pre-arm line count (expected 0) in the gate.
- **Instrumentation:**
  1. **Postgres `log_statement='all'`** with `log_line_prefix='%m [%p] h=%h '` —
     the campaign's established per-tick DB instrument (W2-1 → W2-6). Here it is
     mandatory, for three statements that have no log line anywhere else:
     `DELETE FROM agents WHERE id = $1` (the twin's deregistration),
     `INSERT INTO agents(id, hostname, …) … ON CONFLICT (id) DO UPDATE` (the
     claim-poll upsert that *undoes* that delete), and `ListStuckRunIDs`' own
     `SELECT r.id FROM runs r LEFT JOIN agents a` (the reaper sweep grid).
     Revert at teardown and say so.
  2. **A continuous 1 s sampler** (§"The detector") — the only instrument that
     can see the agent row's absence window, which is the whole of Part D.
  3. **Per-container agent logs.** `claimed_by` is the string `agent1` for a run
     claimed by *either* process, so **the DB cannot attribute a run to a
     process**. `docker compose logs --no-log-prefix agent1` vs `… agent1b` is
     the only attribution instrument in the system. Every run id in this scenario
     must be attributed that way before anything is claimed about it.
  4. **Controller logs**, for `"agent reconcile: failed orphaned run (agent
     process replaced)"` (`internal/controller/api_agent.go:828`) and
     `"stuck-run reaper: failed orphaned run (agent lost)"`
     (`internal/controller/stuckrun_reaper.go:64`). Note the asymmetry that
     drives Part B's attribution: **the heartbeat reconcile path logs nothing on
     success** — `api_agent.go:101-119` only emits on `failOrphanedRun` *error*
     (`slog.Warn` at `:115`). So a run failed by a heartbeat reconcile is
     attributable only by *elimination* plus `runs.updated_at` and
     `step_reports`.
- **Evidence root / drivers.** Captures from the 2026-07 execution are cited
  by relative name (`w2-7/...`). They resolve against the campaign evidence
  root, which is **not in this repository**: `<project parent>/edgecase-evidence/`,
  a sibling of the checkout (`test/edgecase/README.md` § "Raw evidence"). The
  drivers this runbook names are in the repo, under `test/edgecase/tools/w2/`.

## Verified mechanism (read before running; do not re-derive)

> **CORRECTED AFTER THE 2026-07-30 RUN — four claims below are wrong, and the
> first is load-bearing for how the whole scenario should be read. Read the
> "Execution notes" at the end of this file before re-running.**
>
> 1. **"The same zombie shape as W1-5" is right in kind and wrong by one to two
>    orders of magnitude for a connected agent.** The literal deliverable held —
>    the run was `Failed` while `agent1` still executed, for **1.798 s** — but the
>    open-ended exposure the W1-5 comparison implies does not exist here. Every
>    executing process was fenced within one `CancelPollInterval` (5 s,
>    `internal/agent/orchestrator.go:37`) of the terminal write — measured
>    **0.939-4.938 s** across six terminal writes. `RunClaim` runs a cancel
>    poller that `GetRun`s every 5 s and cancels the run's ctx on any terminal
>    status (`orchestrator.go:124-152`). W1-5's 40.2 s / 162.5 s zombies were
>    produced by a *partition*, which is what made its poller blind. So the harm
>    in Parts A/B/D is **destroyed work**, not duplicated side effects, and I6's
>    measurement is a *soft fence*, not an absence of one. Filed as a W2-7
>    observation.
> 2. **`MarkRunStepsInterrupted` writes `status = 'Failed'`, not
>    `'Interrupted'`** (`internal/store/postgres.go:819-830`). The Part A
>    deliverable that said "expect `Interrupted`" is wrong. The usable
>    fingerprint of this path is `status='Failed'` with **`exit_code` NULL** and
>    `ended_at` = the reap instant.
> 3. **The gate's `ALTER SYSTEM` invocation as first written cannot work.** Two
>    `ALTER SYSTEM` statements inside one `psql -c` are a single implicit
>    transaction and Postgres answers `ERROR: ALTER SYSTEM cannot run inside a
>    transaction block`; `pg_reload_conf()` then succeeds and a new session still
>    reports `log_statement = none`, which looks exactly like the instrument
>    working. **One `-c` per `ALTER SYSTEM`.** Fixed in the gate and teardown
>    below (and in the W2-6 runbook, which carries the same broken form).
> 4. **The route is `POST /api/v1/agents/{id}/runs/reconcile`**
>    (`internal/controller/server.go:250`), not `.../reconcile-runs`. The Task 8
>    brief and an earlier draft of this file used the latter.
>
> Two predictions that DID hold, for the record: Part D landed on the **first**
> aimed attempt (predicted sweep instant `02:53:36.53` vs actual `02:53:36.543`),
> and the twin's fallback onto `agent1`'s persisted credential worked exactly as
> §"CREDENTIALS" in the overlay predicted.

### (1) Why two processes with one ID is a supported configuration by omission

| Surface | File:line | What it does |
|---|---|---|
| `agents` schema | `internal/store/migrations/001_init.up.sql:18-27` | no session id, no generation, no fencing token — `id` is the only key |
| registration | `postgres.go:1083-1108` (`UpsertAgent`), `:1118-1142` (`UpsertAgentOnClaim`) | both `ON CONFLICT (id) DO UPDATE`; a second process silently overwrites `hostname`/`os`/`version` and unions `labels` |
| heartbeat | `postgres.go:1144-1147` (`TouchAgent`) | bare `UPDATE agents SET last_seen_at = NOW() WHERE id = $1` — **does not insert**, so it cannot resurrect a deleted row |
| per-run ownership | `internal/controller/agent_guard.go:121` | `run.ClaimedBy == "" \|\| run.ClaimedBy != agentID` — a bare string compare |
| reconcile authorisation | `api_agent.go:815-832` | `handleAgentReconcileRuns` takes the agent id from the **URL path** and calls **no** guard at all |

So the collision is not merely unprevented, it is **unobservable**: one row, one
`last_seen_at`, whichever process wrote last. That invisibility is the
operationally dangerous part and is the root cause both Part A and Part B
inherit.

### (2) Three orphan definitions, and which one each part exercises

Do not conflate these — W2-2 shipped a draft with the first two inverted.

| Path | Query | Grace | Trigger |
|---|---|---|---|
| **startup reconcile** | `ListRunningRunIDsByAgent`, `postgres.go:266-285`, bare `SELECT` at **`:271`** | **none** | `POST /api/v1/agents/{id}/runs/reconcile`, once per process start (`internal/agent/agent.go:204-215`), wrapped in `retryUntilSuccess` |
| **heartbeat reconcile** | `ListReconcilableRunIDsByAgent`, `postgres.go:287-309`, predicate at **`:294`** | `claimed_at < NOW()-60s` (`heartbeatReconcileGrace`, `api_agent.go:71`) | every heartbeat that carries a body; fails every listed run **absent from that process's own `activeRunIds` snapshot** |
| **stuck-run reaper** | `ListStuckRunIDs`, `postgres.go:1238-1248` | `claimed_at < NOW()-60s` **and** (`a.id IS NULL` **or** `last_seen_at < NOW()-90s`) | 30 s tick, leader-elected |

Part A rides the first, Part B the second, Part D the third.

The agent's startup order matters and is fixed: `Register` (`agent.go:186-196`)
→ `ReconcileRuns` (`:204-215`) → slots + `StartHeartbeat` (`:277`). So a twin
reconciles **before** it can claim anything, and an early ctx cancellation
during the reconcile retry returns at `:216` **without** reaching the `Deregister`
at `:344-350`.

### (3) Part A's mechanism — the ungraced startup reconcile

`ListRunningRunIDsByAgent` is `SELECT id FROM runs WHERE claimed_by = $1 AND
status = 'Running'` with no time clause of any kind. A second process starting up
therefore fails **every** Running run under the shared ID, including one claimed
a second earlier, and `failOrphanedRun` (`stuckrun_reaper.go:76-90`) is the same
three-write sequence the reaper uses: `MarkRunStepsInterrupted` →
`MarkRunFinished` (which also releases mutex/named-lock rows) →
`cancelDescendantRuns`. Nothing tells the *executing* process anything, so the
zombie shape is W1-5's **in kind — but see correction 1 above: on a connected
agent the window is one 5 s `CancelPollInterval`, not tens of seconds**.

The function's own doc comment (`api_agent.go:808-814`) says a restarted process
"no longer executes runs its previous incarnation claimed" — true of a restart,
**false of a twin**, and it is an unexported handler's own comment, so per the
campaign rule (`FINDINGS.md:479`) it is not a citable contract. Note it as the
assumption the design rests on, not as a violated promise.

### (4) Part B's mechanism — mutual annihilation via heartbeat

Each process's heartbeat reports only the runs *it* has in flight
(`activeRuns.Snapshot`, `agent.go:277`), and the controller fails every Running
run under the shared ID older than 60 s that is absent from that snapshot. With
two processes each executing one run, each heartbeat's snapshot omits the other's
run — so each process's heartbeat kills the other's work. Heartbeat interval is
15 s (`internal/agent/heartbeat.go:10`), so the first reconcile lands within 15 s
of the 60 s grace expiring.

`MaxConcurrent` defaults to 1 (`agent.go:217-220`), so one run per process is the
natural steady state and no third run is needed.

**Attribution, since this path is silent.** Establish it by elimination, and
evidence each limb:
- not the **startup** reconcile — that logs `api_agent.go:828` on every run it
  fails; grep for it and show the count is unchanged across the window;
- not the **stuck-run** reaper — it logs `stuckrun_reaper.go:64` per run and
  `:66` per sweep, and in any case cannot match: the `agents` row exists and its
  `last_seen_at` is refreshed every 15 s by two processes, so both disjuncts of
  `(a.id IS NULL OR a.last_seen_at < NOW()-90s)` are false. Sample
  `NOW()-last_seen_at` across the window to prove it;
- not the **queued-run** reaper — it only selects `Queued` (`postgres.go:1275-1286`);
- not a **cancel** — no cancel is issued; check the audit log
  (`GET /api/v1/audit`) for `run.cancel` and show zero;
- therefore the heartbeat reconcile, corroborated positively by `runs.updated_at`
  landing in a 15 s heartbeat slot rather than on the reaper's 30 s grid, and by
  the `step_reports` fingerprint `MarkRunStepsInterrupted` leaves — **which is
  `status = 'Failed'` with `exit_code` NULL and `ended_at` = the reap instant,
  NOT `'Interrupted'`** (`postgres.go:822` writes `'Failed'`; the string
  `'Interrupted'` appears nowhere in `internal/`). Querying for `'Interrupted'`
  returns zero rows and reads exactly like "the reap path was never taken",
  which is a false negative on this scenario's major. `'Failed'` alone does not
  distinguish a reap from a cancel — the NULL `exit_code` plus the zero
  `run.cancel` audit rows above are what do.

### (5) Part D's mechanism — the natural path to W2-3's major

W2-3 established, by direct `DELETE`, that once the `agents` row is gone the
reaper's `a.id IS NULL` disjunct matches with **no time component**, leaving
`claimed_at < NOW()-60s` as the only gate — so a *healthy, heartbeating* agent's
run becomes reapable at `claimed_at + 60 s`. W2-3 deferred the natural
demonstration to this scenario, because the natural producer of that `DELETE` is
exactly a duplicate-ID sibling deregistering: `Agent.Run`'s SIGTERM path builds a
fresh 5 s context and calls `Deregister` (`agent.go:343-350`), which is an
unconditional `DELETE FROM agents WHERE id = $1` (`postgres.go:1149-1152`) — it
does not check whether another process is still using the ID.

**The exposure window, and why it is short.** `TouchAgent` cannot recreate the
row (bare `UPDATE`, §(1)), so heartbeats do not heal it. The only healer is
`UpsertAgentOnClaim` at `api_agent.go:150`, called once per claim **HTTP
request**. The agent runs 1 execution slot plus a **16-slot detached claim pool**
(`agent.go:314-327`, `d = 16` when unset), each slot long-polling with
`timeout=30s` (`agent.go:410-414`), so the row is recreated on the next
claim-poll turnover — within ~30 s, and much sooner if the pollers have drifted
apart.

**Therefore Part D is a phase-locking problem with four constraints**, and the
runbook below satisfies them by construction:

1. the run `R` must be claimed by **`agent1`**, not by the twin (else the twin
   cannot be stopped without SIGKILLing a run in flight, and a SIGKILLed process
   never reaches `Deregister` — no `DELETE`);
2. the twin must be **idle** when stopped, so its drain returns immediately
   (`--drain-timeout` defaults to wait-forever, W2-2);
3. the twin must be gone **before its first heartbeat after `claimed_at + 60 s`**,
   or Part B's mechanism kills `R` first and Part D measures nothing. Heartbeats
   are 15 s apart, so treat `claimed_at + 60 s` as the hard deadline;
4. the `agents` row must still be **absent at a reaper sweep instant later than
   `claimed_at + 60 s`**. Sweeps sit on a 30 s grid (W2-3/W2-4: 2-3 executions
   clustered within ~10 ms, then ~30 s of silence), so the absence window must
   cover ≥ one grid position past the grace.

(3) and (4) pull against each other: the `DELETE` must land before
`claimed_at + 60 s` but the *useful* part of the ≤30 s absence window is only
what follows it. The runbook resolves this by measuring `agent1`'s claim-poll
upsert grid first and then choosing the trigger instant so that the last upsert
before `claimed_at + 58 s` falls late in that interval. **Restart `agent1`
immediately before this part** so all 17 of its pollers start together and the
grid is tight; a long-lived process's pollers drift apart and the gaps shrink.

**If the gaps turn out to be too small to aim at, that is the result.** It bounds
W2-3's exposure far more tightly than "~30 s" and must be reported as a
measurement of the upsert cadence, not as "the path is unreachable".

### (6) Part C's mechanism — the ownership guard

`agentRunGuard` (`agent_guard.go:100-131`) resolves ownership as
`run.ClaimedBy != agentID` against a string, with a `claimedBy` memo cache in
front of it. Both processes authenticate as `agent1` and both write to
`/api/v1/agents/agent1/…`, so **the guard cannot distinguish them even in
principle**. Two demonstrations, of different strength:

- **Code-read, unarguable:** `handleAgentReconcileRuns` (`api_agent.go:815-832`)
  calls no guard at all — the agent id comes from the URL path. Parts A and B
  *are* the live demonstration of that, since they show one process terminating
  runs it never executed.
- **Live, direct:** obtain an access token for `agent1` from a **third**,
  independent client and write to a run one of the two agent processes is
  currently executing. `agent1`'s refresh credential is a file in the
  `agent-credentials` volume, readable from the `agent-enroll` service (which
  mounts the same volume and has a shell), and
  `POST /api/v1/agents/token/refresh` with it as the bearer returns an access
  token (`api_agent_enrollment.go:407-445`). With that token, `POST
  /api/v1/agents/agent1/logs` and `…/steps` for the victim run id pass the guard.
  **Cost, which must be disclosed:** the refresh **rotates**, so this consumes
  the credential the two live processes share. Do it **last**, after Parts A/B/D,
  and record what the two processes do afterwards. `agentRefreshOverlap` keeps
  the superseded token usable for a grace window, so the expected outcome is "no
  visible effect within the session".

### (7) What the docs promise (search before filing)

- `grep -rn -i "same agent id\|duplicate agent\|two agents" docs/` and
  `grep -rn -i "fencing\|deregister" docs/` **before** filing anything. W2-1
  already established that agent deregistration is entirely undocumented
  (`grep -rn -i deregister docs/` matches nothing), which means the
  Part D root cause has no contract to contradict and rests on **I1** plus the
  W2-3 entry it completes.
- `docs/agents.md` / `docs/operations.md` — check whether the agent id is
  described as an identity, a name, or a lock. If nothing states uniqueness is
  required, that absence is itself the minor documentation finding, filed as an
  observation, not stretched into a violation.

## The detector (runs continuously, across every arm)

One row per second. The `agents` row's presence/absence is invisible to every
other instrument, and Part D lives or dies on it.

```bash
SCRATCH=<scratchpad>/w2-7 ; mkdir -p "$SCRATCH"
while :; do
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "
    SELECT to_char(NOW(),'HH24:MI:SS.MS')
        || '|agents=' || (SELECT count(*) FROM agents WHERE id='agent1')
        || '|seen=' || COALESCE((SELECT round(EXTRACT(EPOCH FROM (NOW()-last_seen_at))::numeric,2)::text
                                 FROM agents WHERE id='agent1'),'-')
        || '|' || COALESCE((SELECT string_agg(
                 left(id,8) || ':' || status || ':' || to_char(claimed_at,'HH24:MI:SS')
                 || ':u' || to_char(updated_at,'HH24:MI:SS.MS'), ' ')
               FROM runs WHERE status IN ('Pending','Queued','Running')
                  OR updated_at > NOW() - interval '4 minutes'),'-');"
  sleep 1
done > "$SCRATCH/detector.txt" 2>&1 &
echo $! > "$SCRATCH/detector.pid"
```

Per W2-5, the sampler must not also perform injection or teardown, and per W2-6
it **must be killed explicitly** (`kill $(cat "$SCRATCH/detector.pid")`) *before*
`down -v`, or it appends `service "postgres" is not running` forever. Every row
carries `NOW()`, so check the tail for gaps before recording any number from it.

## Baseline gate

Confirm all of these before recording anything. If any fails, STOP and report
BLOCKED with the evidence.

```bash
# G0. Clean the shared side-effect log and prove it.
rm -f ../edgecase/sideeffect-data/sideeffect.log
wc -l ../edgecase/sideeffect-data/sideeffect.log 2>&1 | tee "$SCRATCH/gate-sideeffect.txt"

# G1. Stack up, agent2 down, twin NOT started.
docker compose $COMPOSE_FILES up -d --build
docker compose $COMPOSE_FILES stop agent2
docker compose $COMPOSE_FILES --profile dup ps --format '{{.Service}} {{.State}}' | tee "$SCRATCH/gate-ps.txt"
curl -s -o /dev/null -w 'readyz=%{http_code}\n' localhost:18080/readyz
# Exactly ONE agent row, id agent1, and confirm its advertised capabilities
# (W2-4: these agents advertise ["native","container"], not ["native"]).
curl -fsS localhost:18080/api/v1/agents -H "Authorization: Bearer ha-admin-token" | tee "$SCRATCH/gate-agents.json"

# G2. Statement logging on. log_parameter_max_length is -1 on postgres:16-alpine
#     (W2-1), so DETAIL parameter lines come free.
#     ONE -c PER `ALTER SYSTEM`: two in a single -c form one implicit transaction
#     and Postgres refuses with `ALTER SYSTEM cannot run inside a transaction
#     block`, after which pg_reload_conf() still returns t and a new session
#     still reports `none` — a silent no-op that looks like success.
docker compose $COMPOSE_FILES exec -T postgres psql -U unified \
  -c "ALTER SYSTEM SET log_statement='all'" \
  -c "ALTER SYSTEM SET log_line_prefix='%m [%p] h=%h '" \
  -c "SELECT pg_reload_conf()"
# SHOW in the SAME session still reports the old value (W2-6) — use a new one.
psql "SHOW log_statement;" ; psql "SHOW log_parameter_max_length;"   # expect all / -1

# G3. Exactly one scheduler leader (0x65786364 = 1702388580 decimal — W2-6 got
#     this conversion wrong once and read 0, which looks like a dead scheduler).
psql "SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid=1702388580;"  # expect 1

# G4. Apply the three jobs.
for f in longrun sideeffect mutex-successor; do
  curl -fsS -X POST localhost:18080/api/v1/jobs \
    -H "Authorization: Bearer ha-admin-token" -H 'Content-Type: application/json' \
    --data-binary @../edgecase/workloads/$f.payload.json -o /dev/null -w "$f=%{http_code}\n"
done | tee "$SCRATCH/gate-jobs.txt"    # expect 200 x3

# G5. Host <-> DB clock skew (injection instants are host clock, everything else DB clock).
psql "SELECT NOW();" ; date -u +%FT%T.%3NZ

# G6. Locks start clean.
psql "SELECT count(*) FROM mutex_holders;" ; psql "SELECT count(*) FROM named_lock_slots;"
```

**Gate G7 — the control, and it must be a real one.** Before any twin exists,
trigger `edge-longrun`, let it run **≥ 150 s** (2.5× the 60 s grace and 5× the
30 s reaper grid), and confirm it stays `Running` with `updated_at` advancing and
no reconcile of any kind. W2-5 was corrected for calling a control "uninjected"
when the injection was still armed: here the control is only valid if `agent1b`
has **never been started** in this stack's lifetime, which `--profile dup ps`
evidences. Cancel it at the end of the window and confirm the cancel is the only
thing that terminated it.

## Part A — the ungraced startup reconcile (fast, deterministic)

### A1 — twin introduced against a mature run

```bash
RID=$(curl -fsS -X POST localhost:18080/api/v1/runs -H "Authorization: Bearer ha-admin-token" \
  -H 'Content-Type: application/json' -d '{"jobName":"edge-longrun"}' | tr ',' '\n' | grep -o '"id":"[^"]*"')
date -u +%FT%T.%3NZ | tee "$SCRATCH/partA1.txt"
# Wait for Running, and record claimed_at from the DB (never from readyz — W2-3).
psql "SELECT id, status, to_char(claimed_at,'HH24:MI:SS.MS'), claimed_by FROM runs WHERE id='<RID>';"
# ATTRIBUTE it: only agent1 exists, but capture the proof anyway so the Part B
# and Part D attributions are comparable.
docker compose $COMPOSE_FILES logs --no-log-prefix --since 2m agent1 | tail -20
# Then start the twin, and stamp the host clock either side of the call.
date -u +%FT%T.%3NZ ; docker compose $COMPOSE_FILES up -d --build agent1b ; date -u +%FT%T.%3NZ
```

Deliverables:

1. `RID`'s terminal status and the **exact** `updated_at`, against `claimed_at` —
   i.e. the age of the run when it was killed, measured, not asserted.
2. The controller log line
   `"agent reconcile: failed orphaned run (agent process replaced)"` with its
   `runId`/`agentId` fields, and the twin's own
   `"failed orphaned runs left by previous agent process" count=1`
   (`agent.go:210`).
3. **The zombie limb (I6, measured not judged).** `agent1`'s container log must
   keep emitting `tick N` after `RID` went `Failed`. Report the last tick number
   and its timestamp, and the span from the terminal write to the process's own
   stop. Also record what `agent1` does when it finally reports: per
   `api_agent.go:513-521` a step report against a terminal run answers **200
   with `alreadyFinalized:true`** without touching `step_reports`, and
   `handleAgentFinishRun` returns early at `:613-621` on the CAS miss — so
   confirm the run's recorded status is never corrected.
4. `step_reports` for `RID`: expect **`status = 'Failed'` with `exit_code` NULL
   and `ended_at` = the reap instant** — that is what `MarkRunStepsInterrupted`
   writes (`postgres.go:822`), despite the function's name. **Do not query for
   `'Interrupted'`**: that string is written nowhere in `internal/`, so the
   query returns zero rows and looks like the reap path was never taken.
   `'Failed'` on its own does not separate a reap from a cancel — pair it with
   the NULL `exit_code` and with §(4)'s zero `run.cancel` audit rows.
5. The twin's credential path, from its own log: expect
   `"enrollment token rejected (expired or already consumed); continuing with the
   existing credential"`. If instead it exits, the overlay is wrong and the whole
   scenario is BLOCKED — say so rather than patching around it.

### A2 — the sharpest version: a run killed seconds after being claimed

With the twin already up, trigger a fresh `edge-longrun`, wait for `claimed_at`,
then `docker compose $COMPOSE_FILES restart agent1b`. Per W2-2 a bare `restart`
is a **SIGKILL after ~1.013 s**, not a graceful replacement — say so; here that
is adequate, because all we need is a second process start, and it makes the
turnaround short. Report the measured `updated_at − claimed_at`. This is the
number that shows the startup reconcile has no grace *by construction*
(`postgres.go:271`), and it is the direct analogue of W2-2's Part B.

**Judge A2's severity on the invariant, not on the surprise.** `postgres.go:271`
has no grace by design and `api_agent.go:808-814` explains why; the *defect* is
not the missing grace, it is that a **live** process's run is terminalized on no
evidence the work stopped — an **I1** violation on the accepted W2-3 precedent
(`FINDINGS.md:687`), with I7 as a second limb. **CORRECTED AFTER THE RUN: do not
frame this as a broken documented contract.** `docs/high-availability.md:408-411`
is scoped entirely to the *replaced-process* case, and its one behavioural
sentence (`:414-415`, "the controller fails every `Running` run still claimed by
that agent ID") sanctions the observed behaviour — cite the invariant, not the doc.

## Part B — heartbeat mutual annihilation (the steady state)

Both processes live and idle. Trigger two runs a few seconds apart so each
process's single execution slot takes one:

```bash
# Trigger R1, wait for Running, attribute it; then trigger R2, same.
# ATTRIBUTION IS MANDATORY AND IS NOT AVAILABLE FROM THE DB: claimed_by is
# 'agent1' for both. Use the per-container logs.
docker compose $COMPOSE_FILES logs --no-log-prefix --since 3m agent1  | grep -n "runId\|tick" | tail
docker compose $COMPOSE_FILES logs --no-log-prefix --since 3m agent1b | grep -n "runId\|tick" | tail
```

Use `edge-sideeffect` for one of the two so the mutex and the shared append-only
log are both in play, and `edge-longrun` for the other. `edge-sideeffect` holds
`edge-mutex`, so it must be the **first** trigger — if both were
`edge-sideeffect` the second would sit `Pending` on the mutex and never be
claimed (`tryQueueRun`, `postgres.go:482+`, rolls back on the `mutex_holders`
unique violation leaving the run `Pending` and emitting nothing — W2-9's
mechanism).

Then wait out the 60 s grace and up to one 15 s heartbeat, and record:

1. **Both runs Failed**, with `updated_at` for each, and each run's age at death.
2. **Both processes still executing** — `tick N` / `run,N,…` lines after both
   terminal writes, per process, with timestamps. (I6.)
3. **The attribution chain of §(4)**, each limb evidenced: the
   `api_agent.go:828` count unchanged, the `stuckrun_reaper.go:64`/`:66` count
   unchanged, `NOW()-last_seen_at` sampled small throughout, zero `run.cancel`
   audit rows, `step_reports.status='Failed'` with NULL `exit_code` (the
   `MarkRunStepsInterrupted` fingerprint — **not** `'Interrupted'`, see §(4)),
   and `updated_at` landing off the reaper grid.
4. **I3, in the unusual direction.** `mutex_holders` must be empty after the
   `edge-sideeffect` run is failed — while its executor is still appending to
   `/data/sideeffect.log`. Then trigger `edge-mutex-successor` and show it
   acquires and prints `acquired-mutex-ok`. File this as a **premature release**,
   not a leak: I3's letter is satisfied and its spirit is not, and the entry must
   say exactly that.
5. **I2, with the instrument I2 asks for and with its limits stated.**
   `/data/sideeffect.log` is a shared host bind across every agent process, so
   count its lines and check for interleaving between the zombie's continuing
   appends and the successor's. Be precise about what this does and does not
   show: the zombie's appends are **one execution continuing past a false
   terminal state**, not a second execution of the same step. An I2 violation
   here needs two *executions*, which this arm does not produce.

## Part C — the ownership guard (run LAST; it consumes the shared credential)

### C1 — the code-read limb (already demonstrated by Parts A and B)

State it with `file:line` and label it code-read: `handleAgentReconcileRuns`
(`api_agent.go:815-832`) applies **no** ownership guard, and `agentRunGuard`'s
check is the string compare at `agent_guard.go:121`. Parts A and B are the live
evidence that one process terminates another's runs.

### C2 — the live limb: a third client writes to a run it is not executing

```bash
# C2a. Read agent1's shared refresh credential (agent-enroll mounts the same volume).
docker compose $COMPOSE_FILES run --rm --no-deps -T agent-enroll \
  sh -c 'cat /var/lib/unified-cd-agent/agent1/credentials.json' | tee "$SCRATCH/partC-cred.json"
# C2b. Exchange it for an access token. NOTE: this ROTATES the credential.
REFRESH=<refreshToken from C2a>
docker compose $COMPOSE_FILES run --rm --no-deps -T agent-enroll \
  sh -c "wget -qO- --header='Authorization: Bearer $REFRESH' --post-data='' \
    http://nginx:8080/api/v1/agents/token/refresh" | tee "$SCRATCH/partC-token.json"
```

With a fresh `edge-longrun` live and attributed to one of the two processes,
POST from this third client:

```bash
# C2c. A log line for a run this client never claimed.
#      Body: api.LogAppendRequest (internal/api/types.go:213-219).
curl -sS -o /dev/null -w 'logs=%{http_code}\n' -X POST \
  localhost:18080/api/v1/agents/agent1/logs -H "Authorization: Bearer $ACCESS" \
  -H 'Content-Type: application/json' \
  -d '{"runId":"<RID>","stepIndex":0,"stream":"stdout","timestamp":"<now RFC3339>","line":"w2-7-third-process-marker"}'
# C2d. A step report for the same run. Body: api.StepReportRequest (types.go:180-193).
curl -sS -w '\nsteps=%{http_code}\n' -X POST \
  localhost:18080/api/v1/agents/agent1/steps -H "Authorization: Bearer $ACCESS" \
  -H 'Content-Type: application/json' \
  -d '{"runId":"<RID>","stepIndex":0,"stageIndex":0,"stepName":"tick","status":"Running"}'
# C2e. Proof it landed in the victim's own stream, interleaved with real output.
curl -fsS "localhost:18080/api/v1/runs/<RID>/logs" -H "Authorization: Bearer ha-admin-token" \
  | grep -n -B2 -A2 'w2-7-third-process-marker' | tee "$SCRATCH/partC-marker.txt"
```

**Optional, destructive, and to be decided at execution time:** `POST
/api/v1/agents/agent1/runs/<RID>/finish` with `{"status":"Succeeded"}` from the
same third client terminalizes a run mid-step from outside its executor. If run,
report the response code and the run's resulting status. If not run, say so and
why — do not imply it was tested.

Record afterwards whether either agent process shows an auth failure in the
following minutes (the rotation cost of C2b), and whether the deregistration/
claim traffic continues normally.

## Part D — the natural path to W2-3's major (the stuck-run reaper)

**Preconditions:** Parts A/B finished; no `Running` run; both processes up; the
detector running with no gaps.

```bash
# D1. Re-align agent1's claim pollers so the upsert grid is tight, and stamp it.
#     (A bare restart is a SIGKILL after ~1.013 s — W2-2. That is fine here: the
#     point is a fresh process whose 1 + 16 pollers all start together.)
date -u +%FT%T.%3NZ ; docker compose $COMPOSE_FILES restart agent1 ; date -u +%FT%T.%3NZ

# D2. Measure BOTH grids from the statement log over >= 2 minutes, with no runs.
docker compose $COMPOSE_FILES logs postgres > "$SCRATCH/pg-partD-grids.log"
grep -nE "INSERT INTO agents" "$SCRATCH/pg-partD-grids.log" | tee "$SCRATCH/partD-upserts.txt"
grep -nE "FROM runs r" "$SCRATCH/pg-partD-grids.log"          | tee "$SCRATCH/partD-sweeps.txt"
```

Report the upsert **cluster** structure (count per cluster, within-cluster
spread, between-cluster gap) exactly as W2-3 did for the sweeps, and the sweep
grid's `(epoch mod 30)` position. Both are needed to aim. **Note that the DB log
cannot separate `agent1`'s upserts from `agent1b`'s** — they are byte-identical
statements and `h=` is the *controller's* address, not the agent's. So measure
the grid with the twin **stopped**, then start it.

```bash
# D3. Bring the twin up (no runs exist, so its startup reconcile is a no-op —
#     confirm 'failedRuns' 0 / no api_agent.go:828 line).
docker compose $COMPOSE_FILES up -d agent1b

# D4. Trigger R so that claimed_at lands a few seconds AFTER an upsert cluster,
#     which per §(5) puts the last cluster before claimed_at+58s late in that
#     interval and maximises the useful absence window. Then attribute R.
#     If the TWIN claimed it, cancel and retry (the twin must be idle at D5).
# D5. Stop the twin ~1 s after the upsert cluster that precedes claimed_at+58s.
date -u +%FT%T.%3NZ ; docker compose $COMPOSE_FILES stop agent1b ; date -u +%FT%T.%3NZ
```

Then watch the detector. Deliverables:

1. The `DELETE FROM agents WHERE id = $1` instant from the statement log, and its
   lag behind the SIGTERM (W2-1 measured 0.319 s for a healthy agent).
2. The **absence window**: first detector row with `agents=0`, last such row, and
   the `INSERT INTO agents` that closed it — i.e. the window's measured width,
   which is the quantity that bounds W2-3's exposure.
3. Whether any reaper sweep fell inside the window **after** `claimed_at + 60 s`,
   from `partD-sweeps.txt`.
4. If it did: `R` Failed with `"stuck-run reaper: failed orphaned run (agent
   lost)"` (`stuckrun_reaper.go:64`) naming `R`, `agent1` still emitting `tick N`
   afterwards, and `NOW()-last_seen_at` **small** at that instant — the last of
   these is the crux, because it is what makes this "a healthy heartbeating
   agent's run was reaped" rather than "a dead agent's run was reaped".
5. If it did not: report the attempt count, the measured absence window, and the
   sweep grid, and state plainly that the natural path was **not** reproduced in
   N attempts while the mechanism stands on W2-3's induced demonstration plus the
   `DELETE`/upsert measurements here. **Cap the attempts at 6** and report the
   count either way. Do not call the path unreachable.
6. Either way: whether the twin's stop produced any operator-visible signal that
   `agent1`'s registration had been deleted out from under a live process. Expect
   none — `TouchAgent` fails silently (0 rows) and the next claim silently
   re-inserts.

## Teardown

```bash
kill $(cat "$SCRATCH/detector.pid")            # BEFORE down -v (W2-6 left one running)
tail -3 "$SCRATCH/detector.txt"                # confirm the tail is data, not errors
docker compose $COMPOSE_FILES exec -T postgres psql -U unified \
  -c "ALTER SYSTEM RESET log_statement" \
  -c "ALTER SYSTEM RESET log_line_prefix" \
  -c "SELECT pg_reload_conf()"
# Verify in a NEW session — SHOW in the reloading session still reports the old
# value (W2-6).
psql "SHOW log_statement;" ; psql "SHOW log_line_prefix;"
docker compose $COMPOSE_FILES logs postgres > "$SCRATCH/pg-full.log"
docker compose $COMPOSE_FILES logs -t controller1 controller2 controller3 > "$SCRATCH/controllers-full.log"
docker compose $COMPOSE_FILES logs --no-log-prefix agent1  > "$SCRATCH/agent1-full.log"
docker compose $COMPOSE_FILES logs --no-log-prefix agent1b > "$SCRATCH/agent1b-full.log"
cp ../edgecase/sideeffect-data/sideeffect.log "$SCRATCH/sideeffect.log" 2>/dev/null || true
docker compose $COMPOSE_FILES --profile dup down -v
docker compose $COMPOSE_FILES --profile dup ps -a
```

`down -v` drops the `agent-credentials` volume, so the next run re-enrols from
scratch — which is what makes Part C2's credential rotation safe to leave behind.
Revert the Postgres instrumentation anyway and record that you did (campaign
rule).

## Recording (severity guidance)

- **Part A / Part B — a legitimate run failed while its executor kept
  executing = major, on I1**, quoting I1 verbatim and arguing the fit as the
  Invariants section above requires (the state reached is false, not absent).
  Two processes both accepted as the owner of one run = major, on the same
  invariant, with `agent_guard.go:121` and `api_agent.go:815-832` as the
  mechanism.
- **The absence of a fencing token is the root cause, filed once**, with
  `UpsertAgent`/`UpsertAgentOnClaim`'s `ON CONFLICT DO UPDATE` as the reason the
  collision is **silently invisible** to operators — that invisibility is the
  operationally dangerous part and belongs in the entry body, not a footnote.
- **Part A2's no-grace startup reconcile:** judge on the **invariant**, not on a
  documented contract (**CORRECTED AFTER THE RUN** — the HA passage is scoped to
  sequential replacement and its `:414-415` sentence sanctions the behaviour).
  `postgres.go:271` is ungraced by construction and `api_agent.go:808-814`
  states the intent, so the missing grace on its own is as-designed — likely an
  **observation**, with the operational cost stated prominently. What makes it a
  violation limb is that the run belonged to a **live** process (I1, per W2-3 at
  `FINDINGS.md:687`; I7 second), not the missing grace.
- **Part D, if reproduced:** this converts W2-3's major from induced to naturally
  reachable. **Cross-reference W2-3's entry; do not re-file it.** What is *new*
  here is the natural trigger and the measured absence window.
- **Part D, if not reproduced:** file the measured absence window and upsert
  cadence as an **observation** that bounds W2-3's exposure, with the attempt
  count.
- **I3's premature release** is its own entry — and it is not an I3 violation.
  Say which invariant it *would* violate if any, or say plainly that it violates
  none and is filed on operational risk.
- **The zombie spans are I6 measurements**, reported as numbers with no
  pass/fail, per I6's own wording.
- Every entry title that is an observation must contain the word "observation"
  (`FINDINGS.md:481`).
- Uncaptured live observations carry
  `(observed live, raw output not captured to scratchpad)`.
- Every numeric claim must trace to a capture whose time window covers it, and
  derived / inferred / code-read figures must be labelled as such.

## Execution notes (added after the 2026-07-30 run — read before re-running)

- **Outcome: all four parts landed, and Part D landed on the first aimed
  attempt.** Seven `FINDINGS.md` entries (4 violations, 3 observations) — see
  them for every number. Total wall time **28 minutes** on a warm image cache
  (`02:30:50` up → `02:59:07` `down -v` complete), of which ~3 min was the
  uninjected control and ~5 min was Part D's two-grid measurement. Budget 35 min
  cold.
- **The four corrections to the "Verified mechanism" section are at the top of
  this file.** The load-bearing one is the soft fence: this scenario does **not**
  produce W1-5-shaped zombies, because the agent's cancel poller can reach the
  controller.
- **`claimed_by` cannot attribute a run to a process, and this is the single
  biggest practical constraint.** Both processes write the literal string
  `agent1`. Every attribution in the findings comes from
  `docker compose logs --no-log-prefix <service> | grep '"msg":"running"'`, and
  in Part B the two runs happened to split one per process on the first try
  (`edge-sideeffect` → `agent1`, `edge-longrun` → `agent1b`). **Do not assume the
  split; check it, and re-trigger if both landed on one process.** Part D
  additionally *requires* the run to be `agent1`'s, and got it first try — the
  runbook's cancel-and-retry loop was never exercised.
- **Part D's aim worked to 6-13 ms and the method generalises.** Measure two
  grids from the statement log with the twin **stopped** — `agent1`'s claim-poll
  `INSERT INTO agents(...)` clusters (17 requests within 5-17 ms, period
  **30.033-30.040 s**) and `ListStuckRunIDs`' `LEFT JOIN agents a ON
  r.claimed_by` clusters (2-3 within ~14 ms, period **30.002-30.007 s**) — then
  trigger the run so `claimed_at` falls **2-10 s after** an upsert cluster and
  stop the twin ~1 s after the last upsert cluster preceding `claimed_at + 58 s`.
  That puts a ≥25 s slice of the ~28 s absence window past the 60 s claim grace,
  which a 30 s sweep grid cannot miss. Predicted `02:53:36.53` / actual
  `02:53:36.543`; predicted healing upsert `02:53:41.55` / actual `02:53:41.556`.
- **`restart agent1` before Part D is not optional.** All 17 pollers of a freshly
  started process fire together; a long-lived process's pollers drift and the gap
  shrinks. Two independent grid measurements in this session (one after a
  restart, one 5 minutes later with the twin also up) both showed clean 17-wide
  clusters, so the drift is slow — but the restart makes the phase *known*.
- **Both processes' upsert clusters are indistinguishable in the DB log.** The
  statements are byte-identical and `log_line_prefix`'s `%h` is the
  **controller's** address, not the agent's. Separate them by *phase*:
  `agent1` at `02:48:41.165 + 30.038k`, the twin at `02:48:23.424 + 30.037k`.
  Measure the grid with the twin stopped, then start it.
- **`UpsertAgentOnClaim` vs `UpsertAgent` in the log:** the claim path's column
  list is `(id, hostname, os, labels, version, env, last_seen_at)`; the register
  path's has **`capabilities`** in it (`postgres.go:1083-1108` vs `:1118-1142`).
  Grep on the exact list or you will conflate 17-per-30 s claim upserts with
  one-per-process-start registrations.
- **Cluster size is a busy-slot side channel:** 17 upserts when idle, **16**
  while a run occupies the single execution slot. Useful as a cheap cross-check
  that the run really is executing on the process you think.
- **A bare `restart` of an *idle* agent deregisters cleanly, which qualifies
  W2-2's fact.** W2-2 measured ~1.013 s to SIGKILL on a `restart`, but that agent
  was *holding a run* (unbounded drain). An idle agent's SIGTERM path returns in
  milliseconds and `Deregister` always lands: the `DELETE FROM agents` was
  0.301-0.373 s after the host-clock SIGTERM in three statement-log measurements.
  **Consequence: `restart agent1b` deletes the shared `agents` row too** (Part A2
  produced a one-sample `agents=0` window at `02:39:15.452`). If an arm needs the
  row preserved, do not restart either process.
- **The detector is the only instrument that can see the absence window** and it
  behaved well: **931 rows over ~25 min, max gap 1.73 s** with a host-side
  `docker compose exec` loop at `sleep 1` (~1.58 s effective cadence). It
  independently confirmed Part D (**18** consecutive `agents=0` rows — `detector.txt`
  lines 748-765, `02:53:14.255` → `02:53:41.116`; an earlier note said 19 — with the run
  flipping `Running → Failed` between the `02:53:36.362` and `02:53:37.944`
  samples). **It was killed explicitly before `down -v`** and its tail is data,
  not connection errors — W2-6 left one running and polluted its own capture.
- **`psql -tAc` cannot use `left(id, 8)` on a `uuid` column** — `function
  left(uuid, integer) does not exist`. Cast: `left(id::text,8)`. The detector's
  first incarnation died on this instantly; the second uses a `.sql` file copied
  into the container with `docker compose cp`, which also avoids the four-deep
  quote nesting that `sh -c 'psql -c "…'\''…'\''…"'` requires.
- **`bc` is not installed in this Git Bash.** For a wait-until-instant loop use
  `awk 'BEGIN{...}'` with `exit !(a>=b)`, and never a tight busy loop without a
  `sleep 0.05` — the first attempt spun for the full 2-minute tool timeout.
- **`curl -o /dev/null` prints `curl: (23) Failure writing output to
  destination` under `MSYS_NO_PATHCONV=1`.** The `-w '%{http_code}'` value is
  still correct; it is noise, not a failure.
- **Part C's token exchange is cheap and safe on a disposable rig, and it is the
  strongest single piece of evidence in the scenario.** `docker compose run --rm
  --no-deps -T agent-enroll sh -c 'cat /var/lib/unified-cd-agent/agent1/credentials.json'`
  → `POST /api/v1/agents/token/refresh` with the refresh token as the bearer →
  a 1 h access token, and then `logs`/`steps`/`finish` against a run a *different*
  process is executing all answer 2xx. Note that the refresh **rotates**: capture
  the credential once and reuse the access token. Neither live process showed an
  auth failure afterwards. **Redact the two capture files** — they contain the
  rig's (now-dead) token material.
- **Two things Part C's write-up must not understate, both corrected after the
  run.** (i) **The arm does not depend on the duplicate ID.** One agent process
  plus any local reader of `$HOME/.unified-cd/<id>/credential.json` is enough on a
  real deployment; the twin only made the reader convenient. Frame it that way or
  a triager will dismiss it as something a fencing token fixes. (ii) **The forged
  terminal write leaves no audit row at all** — `auditLogMiddleware`
  (`audit.go:163-172`) is mounted only on the four human subrouters
  (`server.go:357`, `:417`, `:426`, `:435`), and the agent identity routes from
  `agentRouteIdentityMatrix` (`server.go:242-261`) carry none. Since Part C's
  invariant is I7, whose text names audit rows, that is part of the violation.
  **This session read the router rather than measuring it** — no capture queried
  `GET /api/v1/audit` after Part C. A re-run should: one admin `GET` after the
  forged `finish` turns a code-read into evidence for the cost of one request.
- **Reading the credential file also revealed that starting the twin had already
  rotated it**: `refreshExpiresAt` equalled the twin's registration instant to the
  millisecond. Two processes sharing one rotating credential file is a real
  hazard past ~40 min (1 h access TTL minus a 15 m + ≤5 m jitter refresh lead);
  it did not bite in a 28-minute session.
- **Do NOT use `edge-sideeffect` for both Part B runs.** It holds `edge-mutex`, so
  the second would sit `Pending` on the mutex and never be claimed. One
  `edge-sideeffect` + one `edge-longrun`, with the `edge-sideeffect` triggered
  first.
- **`agent2` must be stopped for the whole scenario** or the arms become coin
  tosses; it deregisters itself cleanly on `stop` (W2-1) and the agent inventory
  then legitimately shows one row.
- **`--profile dup` is needed for `ps`** but not for `up -d agent1b` /
  `stop agent1b` (naming a service enables its profile). `--profile dup ps -a`
  listing no `agent1b` row at all is the *evidence* that the control window had
  no injection armed — capture it inside the control's own file.
- **Instrumentation was reverted and verified in a fresh session**: `ALTER SYSTEM
  RESET log_statement` / `RESET log_line_prefix` / `pg_reload_conf()`, then
  `SHOW log_statement` → `none` and `SHOW log_line_prefix` → `%m [%p] `
  (`w2-7/teardown.txt`). The stack was torn down with `--profile dup down -v`
  (both volumes and the network removed, `ps -a` empty), and
  `test/edgecase/sideeffect-data/sideeffect.log` was copied to the evidence root
  and then deleted so the next scenario starts from zero.
- **Postgres statement logging cost ~52 MB for 28 minutes** on this stack (~1.7
  MB/min, lighter than W2-6's ~114 MB/50 min because there is no every-minute
  schedule). The four `pg-*.log` captures are gzipped in the evidence root; total
  evidence 2.7 MB.
