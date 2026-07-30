# W2-9 — Pending-snapshot head-of-line blocking

- **Invariants** (quoted verbatim from
  `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:48-54`):
  - **I1 (run accounting)** — "every API-accepted run reaches exactly one
    terminal state; no phantom runs from duplicate fires/webhooks" (`:48`).
    **This is the primary invariant and the fit is on the "exactly one" clause
    read as a lower bound, not an upper one.** The probe run is accepted by
    `POST /api/v1/runs` with a `200` and a run id, and if it is never examined
    by the scheduler it reaches **zero** terminal states — it is not `Failed`,
    not `Cancelled`, not `Succeeded`, and nothing in the system reaps `Pending`
    (see §(3): the queued-run reaper's predicate is `r.status = 'Queued'`,
    `internal/store/postgres.go:1279`). W2-8's I2 attribution was rejected for
    treating "at most once" as if it forbade zero; do **not** make the mirror
    mistake here. I1 says **exactly** one, so zero is as much a breach as two,
    and the entry must say which side it is claiming and why.
    - **The load-bearing question is therefore whether the block is
      indefinite.** On this rig it is not literally forever, because the mutex
      holder is a 600 s `sleep` — so the honest claim is bounded by what is
      measured, and the runbook must measure the probe's actual latency rather
      than assert "never". State the observed window, then state separately
      what the code implies for a holder that does not exit.
  - **I5 (bounded recovery)** — "after fault injection the system returns to
    steady state within documented bounds (leader re-election ≤ seconds;
    stuck-run reap ≤ staleAfter 90s + interval 30s; the bounds in
    `docs/high-availability.md` are the contract)" (`:52`). **Read the wording
    before leaning on it: I5 has two preconditions and this scenario satisfies
    neither cleanly.** (i) It injects **no fault** — no kill, no partition, no
    revocation; the state is produced by ordinary `POST /api/v1/runs` traffic
    against a documented feature. (ii) I5 names
    **`docs/high-availability.md`** as the source of the bounds, and that
    document contains **no** bound on Pending→Queued latency or on queue depth
    (grep it; §"Orphaned-Run Recovery" bounds reaping, §"Horizontal scaling"
    bounds agent loss). So **I5 is at best a secondary, analogical home** and
    the entry must say so rather than quietly promoting it. The same trap fired
    in W2-5 (I3 relabelled) and W2-7 (a contract that sanctioned the
    behaviour). If the measurement shows the system *does* return to steady
    state once the queue drains, that is an I5 **null result** and must be
    reported as one.
  - **NOT I3.** I3 is "mutex/semaphore/concurrency slots are released when the
    holder reaches a terminal state" (`:50`). The mutex here is held correctly
    by a live run and released correctly at its end. Nothing leaks. W2-5 was
    corrected for exactly this relabelling — the presence of a mutex in the
    setup is not an I3 hook.
  - **NOT I2, NOT I4, NOT I6, NOT I7.** No side-effect log is consulted for the
    verdict, no log-integrity claim, no zombie, and the displayed state
    (`Pending`) is *accurate* — the run really is pending. **The absence of an
    I7 hook is itself worth stating:** every surface tells the operator the
    truth, which is precisely why the condition is invisible.

## Verified mechanism — the spec's original premise was inverted; read this first

**The original W2-9 row described the claim query. That is the wrong phase and a
runbook written against it would measure nothing.** Claiming is two phases and
the blocking is in the first one.

| Phase | Function | Predicate | Batch |
|---|---|---|---|
| 1 — enqueue | `TransitionPendingToQueued` (`internal/store/postgres.go:437-475`) | `WHERE status = 'Pending' ORDER BY created_at LIMIT $1` (`:440`) | **`limit = 50`**, hardcoded at the one call site, `internal/controller/scheduler.go:58` |
| 2 — claim | `claimNextRun` (`internal/store/postgres.go:679-724`) | `WHERE r.status = 'Queued' … ORDER BY created_at LIMIT 1 FOR UPDATE OF r SKIP LOCKED` (`:695`) | one row |

**A mutex-blocked run stays `Pending`.** `tryQueueRun` (`postgres.go:482`)
opens a transaction, `SELECT status … FOR UPDATE`s the row, and attempts
`INSERT INTO mutex_holders(mutex_name, run_id)` (`:546-549`). On a unique
violation it returns `false, nil` (`:551-553`) and the `defer tx.Rollback(ctx)`
at `:487` discards everything — **the run's status is never written, no log line
is appended, and nothing is emitted**. `TransitionPendingToQueued` simply does
not increment its counter (`:466-471`), and `scheduler.go:66-68` only logs when
`n > 0`. So the blocked run is silently re-considered and silently re-rejected on
every 200 ms tick, forever, at `Pending`.

**Consequence, and the whole point of this scenario:** because blocked runs never
leave `Pending`, they never enter phase 2's candidate set — so **head-of-line
blocking cannot occur in the claim query at all**. It occurs one phase earlier.
The phase-1 snapshot is *always the 50 oldest `Pending` rows*, and a run at
`created_at` position 51 or later is **never examined**: no `tryQueueRun` call,
no mutex check, no log line. It is not "queued behind" anything — it is outside
the batch.

### (1) The arithmetic threshold is a prediction, not a measurement

`limit = 50` (`scheduler.go:58`) + `LIMIT $1` (`postgres.go:440`) predicts that
the snapshot admits the probe iff the number of `Pending` runs **strictly older
than the probe** is ≤ 49, i.e. iff total `Pending` (probe included) ≤ **50**.

**That number is derived from two source lines and nothing else. Measure it.**
The falsification step below cancels blocked runs one at a time and records the
`Pending` count at the transition. **If the probe unblocks at a count other than
50, that number is the finding** and this model is wrong — report the measured
count either way, with the capture that shows it.

### (2) The same starvation is reachable with no mutex at all

Two independent amplifiers, both code-read:

- **Git-unresolved runs consume snapshot slots identically.** `tryQueueRun`
  returns `false, nil` for any step whose `uses.job` starts with `git://`
  (`postgres.go:513-518`), before any lock is touched, with the same silent
  rollback. So >50 Pending runs awaiting git resolution starve every newer run
  with no concurrency configuration anywhere.
- **The git resolver has its own 50-row oldest-first batch** —
  `ListPendingRuns(ctx, 50, …)` (`scheduler.go:291` → `postgres.go:2064-2069`),
  same `WHERE status = 'Pending' ORDER BY created_at LIMIT $1`. So a deep Pending
  backlog starves *resolution* as well as *enqueue*.

**And the codebase already knows this pattern is a hazard.** `docs/operations.md:53`
(§"Sweep failure backoff"):

> "The log archiver, run-retention sweeper, and git resolver retry a persistently
> failing candidate with exponential backoff (1 min doubling to 1 h) instead of
> letting it occupy the head of every oldest-first batch — a handful of broken
> runs can no longer starve archival, deletion, or resolution for everything
> newer."

**Read the scope before citing it as a contract, per the W2-7 lesson.** That
sentence names **three** sweeps and the scheduler's `TransitionPendingToQueued`
is not one of them; `ListPendingRuns` even takes the `excluded` set that
implements the mitigation, while `TransitionPendingToQueued` takes no such
parameter. So the passage is **not a promise this behaviour breaks** — it is
evidence that the identical failure mode was recognised and mitigated in three
places and left unmitigated in the one on the critical path. Use it for
**severity context**, never as the violated contract.

### (3) Nothing reaps `Pending`

Grep for the status: the only writers are the scheduler's snapshot (`:440`), the
`FOR UPDATE` re-check (`:500`), the git resolver's list (`:2067`) and spec update
(`:2087`). The queued-run reaper's predicate is `r.status = 'Queued'`
(`postgres.go:1279`); the stuck-run reaper's is `Running`. So a `Pending` run has
**no timeout, no grace, and no reaper**. This is what makes the I1 limb possible
and it must be confirmed live (gate G5), not assumed.

### (4) What the docs say about mutexes — and what they do not

`docs/jobs.md:1419-1429` (§"Concurrency Control" → "Mutex"):

> "A named mutual exclusion lock — only one run holding the mutex runs at a
> time. … Runs that cannot acquire the mutex wait in the queue until it is
> released."

The section opens (`:1417`) with "Prevent multiple runs from executing
simultaneously when they share a resource." Three readings, and the entry must
say which it rests on:

1. **As a prohibition on affecting unrelated runs — it is not one.** No sentence
   says a mutex cannot delay a run that does not use it. On this limb the docs
   are **silent, not contradicted**, and the finding rests on I1.
2. **As a scoping statement — it is one, and it is the honest severity limb.**
   "when they *share* a resource" and "Runs that cannot acquire *the mutex*"
   both scope the wait to contenders. An operator reading only this cannot
   predict that `edge-unrelated-probe`, which declares no `concurrency` block at
   all, is affected. Use this for severity, not as the violation.
3. **As a queue-semantics claim — check it, do not assume it.** "wait in the
   queue" implies the blocked runs are *in* a queue; in fact they are `Pending`,
   never `Queued`, so the word "queue" in the docs does not name the `Queued`
   status. Worth one sentence; not a finding.

Also grep before filing, per house rule:
`grep -rn -i "head-of-line\|starv\|fair\|FIFO\|queue depth\|backlog" docs/` and
`grep -rn -i "Pending" docs/high-availability.md docs/operations.md docs/troubleshooting.md`.

## Stack

Plain `test/ha`, **no overlay**. Nothing here needs a shared volume or nginx
surgery. Every compose call is:

```bash
cd test/ha
export COMPOSE_FILES="-f docker-compose.ha.yaml"
export MSYS_NO_PATHCONV=1          # Git Bash rewrites container paths (W2-5)
docker compose $COMPOSE_FILES up -d --build
```

Throughout, `psql` means:

```bash
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "<sql>"
```

and `API` means `curl -sS -H "Authorization: Bearer ha-admin-token"` against
`http://localhost:18080`.

**Exclusive use of `edge-mutex`.** `mutex-hog.payload.json` and
`sideeffect.payload.json` both declare `concurrency.mutex: edge-mutex`, which is
also used by W1 fixtures (`mutex-successor.payload.json`). **Do not run this
scenario concurrently with anything else touching `edge-mutex`** — a foreign
holder silently changes which run is the head of the line.

**Workloads:**

| Fixture | Job | Mutex | Step |
|---|---|---|---|
| `mutex-hog.payload.json` | `edge-mutex-hog` | `edge-mutex` | appends to `/data/mutex-hog.log`, then `sleep 600` |
| `sideeffect.payload.json` | `edge-sideeffect` | `edge-mutex` | 120 × (append + `sleep 1`) |
| `unrelated-probe.payload.json` | `edge-unrelated-probe` | **none** | `echo probe-ran` |

All three are `native: true` with `agentSelector: [kind:linux]`. Applied with
`POST /api/v1/jobs`; triggered with `POST /api/v1/runs` body
`{"jobName":"<job>"}` (`internal/controller/server.go:370` → `handleTriggerRun`;
**there is no `/api/v1/jobs/<job>/trigger` route**). Bulk submission is
`test/edgecase/tools/bulk-submit.sh <job> <count>`, which prints one run id per
line.

**Timing budget — the experiment must finish inside the hog's 600 s hold.** If
the hog exits mid-experiment, a blocked `edge-sideeffect` run acquires the mutex,
`Pending` drops by one for a reason unrelated to any cancel, and the falsification
becomes unattributable. Target timeline from the hog's claim (`t0`):

| Δt | action |
|---|---|
| +0 s | hog claimed, `mutex_holders` verified |
| +30 s | `bulk-submit.sh edge-sideeffect 55` (≈55 sequential POSTs) |
| +60 s | Pending census; confirm the 55 are the oldest |
| +65 s | trigger the probe; poll 180 s |
| +250 s | falsification: cancel one at a time with a census after each |
| +330 s | probe expected unblocked; done with ≈270 s of margin |

Record `t0` and the hog's expected exit; if the margin is lost, **abort and
re-run** rather than reasoning around a second holder.

## BASELINE GATE — do not proceed past a failing check

Write every gate output to `$SCRATCH/gate.txt`.

```bash
SCRATCH="<scratchpad>/w2-9" ; mkdir -p "$SCRATCH"
```

- **G0 — worktree.** `git rev-parse --show-toplevel` is `.../wt-edge-spec`,
  branch `plan/edge-case-w2`. `docker compose ls` shows the developer stack
  (project `unified-cd`) untouched; `test/ha`'s project is `unified-cd-ha`
  (`docker-compose.ha.yaml:1`), so they do not collide.
- **G1 — stack health.** All three controllers `healthy`; `API /readyz` → 200;
  `GET /api/v1/agents` lists **agent1 and agent2**, both connected, and record
  their **labels** — the fixtures select on `kind:linux` and a scenario that
  assumes a selector without reading it has been wrong twice in this campaign.
  Capture the full agent list.
- **G2 — `edge-mutex` is free and stays ours.** `SELECT * FROM mutex_holders;`
  → **zero rows** before the hog is triggered, and
  `SELECT count(*) FROM runs WHERE status IN ('Pending','Queued','Running');`
  → **0**. A non-empty start invalidates every count below.
- **G3 — Postgres statement logging armed, verified in a *fresh* session.**
  **One `ALTER SYSTEM` per `psql -c`** — two in one `-c` is an implicit
  transaction, Postgres refuses it, and `pg_reload_conf()` still returns `t`, so
  the broken form is indistinguishable from success (W2-7, `plan:80`).

  ```bash
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "ALTER SYSTEM SET log_statement='all';"
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "ALTER SYSTEM SET log_line_prefix='%m [%p] h=%h ';"
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "SELECT pg_reload_conf();"
  # fresh session — this is the check that matters
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_statement;"    # must print: all
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_line_prefix;"
  ```

  Record both `SHOW` outputs. **Revert at teardown and say so in the findings**
  (W2-6 shipped a runbook whose revert could not have worked).

  This is the instrument that proves the *silence*: the phase-1 snapshot is
  arg-less-plus-one-parameter and logs as
  `execute stmtcache_<hash>: SELECT id, spec, params FROM runs WHERE status = 'Pending' …`.
  **Both statement forms must be matched** — pgx logs parameterised queries as
  `LOG: execute stmtcache_<hash>:` and arg-less ones as `LOG: statement:`
  (W2-8 note 4). A `statement:`-only matcher will see nothing here.

  **Budget the log volume.** The scheduler ticks every **200 ms** and each tick
  issues the snapshot plus one `SELECT … FOR UPDATE` per candidate — with 55
  blocked candidates that is ~56 statements every 200 ms per winning replica,
  i.e. of order 10³/s. Arm G3 **only for the two short windows that need it**
  (the phase-1 silence proof and the transition), and `RESET` in between;
  otherwise the Postgres log outgrows `docker compose logs`.
- **G4 — jobs applied.** `POST /api/v1/jobs` with all three payloads → 200 each.
  Capture the responses.
- **G5 — nothing reaps `Pending` (the I1 precondition), confirmed live.**
  Trigger **one** `edge-sideeffect` run while `edge-mutex` is held by the hog and
  leave it alone for **≥ 5 min** (longer than `UNIFIED_QUEUED_RUN_GRACE`'s 5 min
  default, `docs/high-availability.md:355-357`). It must still be `Pending`, not
  `Failed`. This is the check that separates "starved" from "eventually reaped",
  and §(3) is code-read until it passes. Run it concurrently with the main
  experiment (it is one of the 55) rather than serially, and say which run id
  carried it.
- **G6 — the probe job actually runs when unobstructed.** Before any bulk
  submission, trigger `edge-unrelated-probe` once on the idle stack and confirm
  `Succeeded` with `probe-ran` in `logs`. Record the **trigger → Succeeded**
  latency; it is the baseline the starved probe is compared against, and without
  it "never claimed" cannot be distinguished from "this fixture is broken".
- **G7 — API 500s.** The API on this rig has been intermittently returning 500s.
  Record, for every gate call, how many attempts it took. `bulk-submit.sh` uses
  `curl -fsS` and `exit 1`s on the first failure, so a 500 mid-batch leaves a
  **partial** submission — if that happens, record the actual count submitted
  and re-derive every threshold from the census, never from the requested 55.

## Part A — the snapshot saturates

**Deliverable:** a census showing ≥51 `Pending` runs that are all older than the
probe, and the probe stuck `Pending` for the whole poll window with an idle
agent.

- **A1 — the holder.** Trigger `edge-mutex-hog`; record `runID_hog`, the trigger
  response time (host clock), `runs.claimed_by`, and `t0` = `runs.claimed_at`.
  Confirm the hold with `SELECT mutex_name, run_id, acquired_at FROM mutex_holders;`
  → exactly one row, `edge-mutex` / `runID_hog`. → `$SCRATCH/partA-hog.txt`.
  **Do not proceed until this row exists** — if the hog is itself starved or
  fails, the whole setup is inverted.
- **A2 — saturate.** `bulk-submit.sh edge-sideeffect 55 | tee $SCRATCH/partA-blocked-ids.txt`.
  Record wall-clock start and end (the 55 sequential POSTs take tens of seconds
  and the `created_at` spread matters for §(1)'s ordering claim).
- **A3 — the census, before the probe.** One read:

  ```sql
  SELECT status, count(*) FROM runs GROUP BY status ORDER BY status;
  ```

  plus the full ordering:

  ```sql
  SELECT row_number() OVER (ORDER BY created_at) AS pos,
         id, job_name, status, created_at
    FROM runs WHERE status = 'Pending' ORDER BY created_at;
  ```

  → `$SCRATCH/partA-census-pre.txt`. Confirm `Pending` **≥ 51** and that every
  `Pending` row is `edge-sideeffect`.
- **A4 — the probe.** Trigger `edge-unrelated-probe`; record `runID_probe` and
  the trigger response (host clock, plus the `200` body's `id`). Confirm from a
  census that it is the **newest** `Pending` row and that its position exceeds
  50. → `$SCRATCH/partA-probe-trigger.txt`.
- **A5 — the poll series.** Poll every 5 s for **180 s**, one line per sample
  with a host timestamp, capturing *both* the API view and the DB view (they can
  disagree and the entry should not rest on one):

  ```bash
  for i in $(seq 1 36); do
    printf '%s ' "$(date -u +%H:%M:%S.%3N)"
    curl -sS -H "Authorization: Bearer ha-admin-token" \
      "http://localhost:18080/api/v1/runs/$runID_probe" \
      | tr ',' '\n' | grep -E '"(id|status)"' | tr '\n' ' '
    echo
    sleep 5
  done | tee "$SCRATCH/partA-probe-poll.txt"
  ```

  **Expected under the hypothesis: `Pending` on every one of the 36 samples.**
  A single `Queued`/`Running` sample falsifies §(1) and is the finding instead.
- **A6 — the idle agent claimed nothing.** At least one agent must be idle for
  the whole window (the hog occupies one; `edge-sideeffect` never starts). Show
  it three ways:
  1. `SELECT id, job_name, status, claimed_by FROM runs WHERE status='Running';`
     → exactly one row, the hog. → `$SCRATCH/partA-running.txt`.
  2. The idle agent's own log over the window
     (`docker compose logs --no-log-prefix agent2 --since 200s`) → no claim, no
     step. **`docker compose logs --since` lags several seconds** (W2-8 note 5)
     — wait ≥6 s and widen the window before concluding anything from it.
  3. The controller HTTP log for `POST /api/v1/agents/agent2/claim`
     (`server.go:247`) over the window: the polls are happening and returning no
     run. → `$SCRATCH/partA-claim-log.txt`.
- **A7 — the silence, from the Postgres log.** With G3 armed for a ~20 s window,
  extract the phase-1 snapshot statements and show that (a) they are firing every
  ~200 ms, (b) each is followed by `SELECT status FROM runs WHERE id = $1 FOR UPDATE`
  for **50** distinct ids, and (c) **`runID_probe` is not among them**. This is
  the direct proof that the probe is not "queued behind" the blocked runs but
  **never examined**. → `$SCRATCH/partA-pglog-snapshot.txt`,
  `$SCRATCH/partA-pglog-ids.txt`. Match **both** pgx log forms (G3).
  - Cross-check with the *absence* of scheduler noise: `docker compose logs
    controller1 controller2 controller3 | grep -c "scheduler enqueued"` over the
    window should be **0** (`scheduler.go:66-68` logs only when `n > 0`).
    Zero enqueues while 56 runs are Pending is the operator-visible symptom:
    **nothing at all is logged.**
- **A8 — operator surfacing.** Record what an operator can see: the probe's API
  body (`status: Pending`, no reason field), `GET /api/v1/runs/{id}/steps`, the
  run's `logs` rows (expect **zero** — `tryQueueRun`'s blocked path appends
  nothing, unlike the queued-run reaper which does, W2-4), and whether any
  metric exposes queue depth or snapshot saturation
  (`curl /metrics | grep -i -E "runs_current|pending|queue"`).
  → `$SCRATCH/partA-surfacing.txt`.

## Part B — falsification: find the real threshold

**This is the part that turns a plausible story into a measured threshold. Do not
skip it and do not batch the cancels.**

- **B1 — cancel one at a time, from the oldest.** For each cancel: `POST
  /api/v1/runs/{id}/cancel`, then immediately read

  ```sql
  SELECT (SELECT count(*) FROM runs WHERE status='Pending') AS pending,
         (SELECT status FROM runs WHERE id='<runID_probe>') AS probe;
  ```

  and again after 2 s (the scheduler ticks at 200 ms, so one tick is well inside
  2 s). One line per cancel with a host timestamp →
  `$SCRATCH/partB-cancel-series.txt`. Cancel from the **oldest** end so the
  probe's position falls by exactly one per cancel.
- **B2 — record the exact transition.** The deliverable is one row of that
  series: the last `Pending` count at which the probe was still `Pending`, and
  the first count at which it was not. **Report the number whether or not it is
  50.** If it is not 50, §(1)'s model is wrong and the measured number is the
  finding; re-read `scheduler.go:58` on the running image
  (`docker compose exec controller1 …` cannot show source — instead diff the
  built commit) before concluding the source was misread.
- **B3 — and then it must actually run.** After the transition, confirm the
  probe reaches `Succeeded` with `probe-ran` in `logs`, and record
  **transition → Succeeded** latency against G6's unobstructed baseline. A run
  that queues but never claims is a *different* finding.
- **B4 — the reverse direction, cheap and decisive.** With the probe already
  `Succeeded`, submit **one more** `edge-sideeffect` (restoring Pending to the
  blocking count) and a **second** probe; confirm the second probe is starved
  again. This shows the threshold is a live property of the current Pending
  count, not an artefact of the first probe's history. →
  `$SCRATCH/partB-probe2.txt`.
- **B5 — bound the starvation honestly.** From the series, state the probe's
  total observed `Pending` duration (trigger → transition) as a **measured
  window**, and state separately, as **code-read**, that nothing in the code
  bounds it: no reaper for `Pending` (§(3)), no snapshot exclusion for
  perpetually-blocked candidates (unlike `ListPendingRuns`'s `excluded`, §(2)).
  Do **not** write "never" for a number you ended by cancelling.

## Part C — the no-mutex amplifier (code-read; execute only if time allows)

§(2)'s git-unresolved path reaches the same state with no concurrency
configuration at all. A live demonstration needs a `uses: git://…` step pointing
at an unresolvable source, which is W2-1/AppSource territory and needs a fixture
this wave does not have. **Record it as code-read** (`postgres.go:513-518`,
`scheduler.go:291`) and say plainly that it was not executed and why. Do not
present it as measured.

## Teardown

```bash
# revert the instrument FIRST, and verify in a fresh session
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "ALTER SYSTEM RESET log_statement;"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "ALTER SYSTEM RESET log_line_prefix;"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "SELECT pg_reload_conf();"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_statement;"   # must print: none
docker compose $COMPOSE_FILES down -v
```

- **Cancel every surviving run before teardown** and confirm `mutex_holders` is
  empty — the hog's 600 s sleep and up to 55 `edge-sideeffect` runs are still
  live otherwise, and `down -v` on a stack mid-run muddies any later re-read of
  the volume.
- **Kill every background sampler before teardown and *capture* that, don't
  assert it** (W2-6 left one running; W2-8's first session asserted it in prose
  only). Keep PIDs in `$SCRATCH/samplers.pid`, `kill` them explicitly, then show
  `jobs` empty and `ps -W | grep -iE "psql|curl|python"` matching nothing, on two
  passes: before the revert and immediately before `down -v`. →
  `$SCRATCH/teardown.txt`.
- Copy `$SCRATCH` into the campaign evidence root at the wave checkpoint
  (`test/edgecase/README.md` § "Raw evidence").

## Recording rules

- **Part A + Part B ⇒ major, primary I1**, if it reproduces: an API-accepted,
  fully runnable run with no concurrency configuration is never examined by the
  scheduler while an agent idles and nothing anywhere says why. Severity argument,
  stated rather than asserted: the run is *correctly* displayed as `Pending`, so
  no record lies (no I7); no work is lost or duplicated (no I2, no I4); but the
  condition is **silent** (zero log lines, zero metrics, zero run-scoped
  explanation), **has no reaper**, and is triggered by ordinary use of a
  documented feature at a threshold no document mentions. That is major, not
  critical: nothing is corrupted and the state self-heals as soon as the backlog
  drops below the threshold — **which Part B demonstrates by doing it**, and that
  self-healing is why the I5 limb is a null result rather than a second violation.
- **Scope the negative half to what was measured.** The hog held for 600 s, so
  the observed starvation window is minutes, not "forever". Say the measured
  window, then say what is code-read about the unbounded case. A reviewer has
  rejected an unqualified "never" once already in this campaign.
- **If Part A does not reproduce** — if the probe is queued during the poll
  window — the entry is an **observation** carrying the measured latency and an
  explicit statement that §(1)'s snapshot model does not predict the behaviour.
  Report the number, not the story.
- **`docs/operations.md:53` is severity context, never the violated contract**
  (§(2)); `docs/jobs.md:1419-1429` is **silent, not contradicted** (§(4)).
- **I5 is secondary and analogical at best** — no fault is injected and
  `docs/high-availability.md` carries no relevant bound. Say so in the entry
  rather than implying I5 supplies a broken numeric promise.
- Entry titles must say **"observation"** for observation entries
  (`FINDINGS.md:481`), and the `Severity` line repeats it as
  `minor (observation)`. A defect found in this campaign's own assets gets an
  explicit `Classification:` line and sits outside both tallies
  (`FINDINGS.md:487`).
- Every number cites a `$SCRATCH` filename whose time window covers it. Derived
  figures say "derived"; code-read figures say "code-read"; uncaptured live
  observations say `(observed live, raw output not captured to scratchpad)`.
