# W2-9 — Pending-snapshot head-of-line blocking

> **CORRECTED AFTER EXECUTION — READ THIS BEFORE THE INVARIANTS BLOCK BELOW.**
> **I1 is NOT the invariant for this scenario and must not be claimed on a
> re-run.** The I1 reasoning set out immediately below was written before the
> measurement and is **wrong**; it is kept verbatim, struck through in effect by
> this note, because the *reason* it is wrong is a trap the campaign keeps
> falling into. Two things killed it. (1) **Both probes reached exactly one
> terminal state** — `Succeeded` at `06:27:52.866707` and `06:43:35.688398`
> (`w2-9/partB4-outcome.txt`) — so the "zero terminal states" premise is false
> on this scenario's own evidence. (2) **I1 has no liveness bound**, so "in zero
> terminal states right now" is true of every non-terminal run at every instant;
> the lower-bound reading proves too much and would make I1 violated by every
> `Pending` row in the system. The only reading that would bite is "never
> reaches one", and that is exactly what cannot be measured here, because every
> starvation window has to be ended deliberately to end the experiment.
> **What the scenario actually rests on:** a contradicted published contract,
> `docs/high-availability.md:163`, **plus I5** — and I5 only on the Part D limb,
> which is the only limb with a fault injection. See correction 1 in the
> execution notes at the end of this file, and the single merged `FINDINGS.md`
> entry.
>
> **The scenario yields ONE major and ONE observation.** An earlier draft filed
> two majors; both described the same root cause (`scheduler.go:58`) and were
> merged. Do not re-split them, and do not count `scheduler.go:58` twice in a
> wave tally.

- **Invariants** (quoted verbatim from
  `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:48-54`):
  - **I1 (run accounting)** — ***SUPERSEDED, see the correction box above; do not
    claim this.*** "every API-accepted run reaches exactly one
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
    `docs/high-availability.md` are the contract)" (`:52`). **PARTLY SUPERSEDED
    — the two-gate test below is right and was kept, but its verdict changed
    once Part D existed. Part D (SIGKILL of the scheduler leader, contradicting
    `docs/high-availability.md:163`) satisfies BOTH gates, so I5 IS claimed
    there and is the invariant that makes that document binding. On the no-fault
    limb (Parts A/B) the verdict below stands unchanged: neither gate holds, and
    I5 is an explicit null result.** The pre-execution reasoning follows. **Read
    the wording before leaning on it: I5 has two preconditions and the Part A/B
    setup satisfies neither cleanly.** (i) It injects **no fault** — no kill, no partition, no
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
- **Evidence root / drivers.** Captures from the 2026-07 execution are cited
  by relative name (`w2-9/...`). They resolve against the campaign evidence
  root, which is **not in this repository**: `<project parent>/edgecase-evidence/`,
  a sibling of the checkout (`test/edgecase/README.md` § "Raw evidence"). The
  drivers this runbook names are in the repo, under `test/edgecase/tools/w2/`.

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
**no timeout, no grace, and no reaper**. **This is not an I1 limb** — I1 was
withdrawn for this scenario (see the correction box at the top of this file): it
has no liveness bound, so "in zero terminal states right now" is true of every
non-terminal run at every instant. What §(3) establishes is that the starvation
window has no code-side upper bound, which is what lets Parts A/B state a
*measured* window plus a *code-read* unboundedness. Confirm it live (gate G5),
do not assume it.

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
   are **silent, not contradicted**. An earlier draft rested the finding on I1
   here; I1 is withdrawn (correction box at the top). What the finding rests on
   is the contradicted published contract at `docs/high-availability.md:163`
   (Part D) plus I5 on that same limb — grep that file for `Pending` directly,
   because this survey's mutex/queue vocabulary misses the sentence.
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
  docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_line_prefix;"  # must print: %m [%p] h=%h
  ```

  **STOP on either mismatch.** Part D's tick/candidate parser keys on `host=`
  from the prefix, so an unarmed `h=%h` silently destroys the attribution that
  every per-replica claim in this scenario rests on.

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
- **G5 — nothing reaps `Pending`, confirmed live** (§(3)'s precondition — not an
  I1 precondition; I1 is withdrawn, see the correction box at the top).
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

## Part D — the post-promotion tick, against a published contract

**RUN THIS FIRST.** It is the scenario's strongest single result and its
cheapest: 51+ accumulated `Pending` runs and one `docker compose kill`. No
probe, no cancel series, no drain — Part A's A1+A2 (hog plus bulk submit) is the
entire setup, and Part D can be taken straight off the back of them before the
probe is ever triggered. It is also the **only** limb with a fault injection,
which is what puts I5 properly in scope; Parts A/B alone breach no invariant and
no contract.

**The contract.** `docs/high-availability.md:163`, under §"What happens during
leader absence" (`:157`):

> "After promotion, the new leader processes any accumulated Pending Runs on the
> next tick — no runs are lost"

Above 50 accumulated this is false: `scheduler.go:58`'s `limit = 50` caps the
snapshot, so the first post-promotion tick processes 50, not "any accumulated".
§(4)'s doc survey misses this sentence because it greps for mutex/queue
vocabulary — grep `docs/high-availability.md` for `Pending` directly.

Two scoping escapes were checked and neither applies: "no runs are lost" is a
**second conjunct joined by an em dash** (a further promise, not a narrowing of
"any accumulated … on the next tick"), and `:159-162` authorises no batch size.

**Driver:** `../edgecase/tools/w2/w2-9-partD.sh` (run from `test/ha` with
`SCRATCH` exported and `MSYS_NO_PATHCONV=1`). Procedure, if running it by hand:

```bash
# D1. Precondition: strictly more than 50 accumulated Pending runs, and the
#     mutex still held so none of them can drain. STOP if pending <= 50 — the
#     contract is not contradicted at or below the limit.
psql "SELECT status, count(*) FROM runs GROUP BY status ORDER BY status;"
psql "SELECT count(*) AS pending FROM runs WHERE status='Pending';"
psql "SELECT mutex_name, left(run_id::text,8) FROM mutex_holders;"

# D2. Identify the current scheduler leader — the only leadership log line there
#     is. This is the kill target.
docker compose $COMPOSE_FILES logs controller1 controller2 controller3 --since 30m \
  | grep -i "scheduler became leader" | tee "$SCRATCH/partD-leader.txt"

# D3. Arm the statement log for a SHORT window only (see §G3's volume budget:
#     ~2,300 lines/s at 200 ms ticks with 50 candidates). Verify fresh-session.
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "ALTER SYSTEM SET log_statement='all';"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "SELECT pg_reload_conf();"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_statement;"   # must print: all
# D7's parser reads host= out of the prefix, so re-confirm the Phase 0 prefix is
# STILL in force here. Both checks are STOP-on-mismatch: with either one wrong
# the tick/candidate measurement below is unattributable and must not be scored.
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_line_prefix;"   # must print: %m [%p] h=%h
sleep 3

# D4. SIGKILL the leader; poll the two survivors for the promotion line.
LEADER=<from D2>
date -u +%FT%T.%3NZ ; docker compose $COMPOSE_FILES kill -s SIGKILL "$LEADER"
for i in $(seq 1 40); do
  L=$(docker compose $COMPOSE_FILES logs controller1 controller2 controller3 --since 3m \
      | grep -i "scheduler became leader" | tail -1)
  [ -n "$L" ] && { echo "PROMOTED: $L (seen $(date -u +%FT%T.%3NZ))"; break; }
  sleep 2
done
sleep 6
psql "SELECT status, count(*) FROM runs GROUP BY status ORDER BY status;"

# D5. Capture and DISARM immediately, verified in a fresh session.
docker compose $COMPOSE_FILES logs --no-log-prefix postgres --since 120s > "$SCRATCH/partD-pglog-raw.txt"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "ALTER SYSTEM RESET log_statement;"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "SELECT pg_reload_conf();"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_statement;"   # must print: none
# This step disarms log_statement ONLY; the prefix stays armed until Teardown.
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_line_prefix;"   # must STILL print: %m [%p] h=%h

# D6. Restore the killed replica so later parts still have >=2 candidates.
docker compose $COMPOSE_FILES start "$LEADER"
```

**D7 — the measurement.** Reduce the raw log to a per-tick candidate count. The
count is the per-candidate `SELECT status FROM runs WHERE id = $1 FOR UPDATE`
tally used as a **1:1 proxy** for the snapshot's row count — `postgres.go:482-489`
shows that `FOR UPDATE` is `tryQueueRun`'s first statement after `BEGIN` with no
earlier return path, so exactly one is issued per snapshot row. It is **not**
read off the snapshot statement, which logs no row count. Say that explicitly in
the write-up. The parser must key on the **extended-protocol** form
(`LOG: execute stmtcache_<hash>: …` plus the following
`DETAIL: parameters: $1 = '<id>'`) — a `statement:`-only matcher sees nothing:

```bash
awk '
/FROM runs WHERE status = .Pending. ORDER BY created_at LIMIT/ {
  if (tick!="") printf "tick %s host=%s : %d candidates\n", tick, host, n;
  tick=$1" "$2; host=$5; n=0; next }
/execute stmtcache_[0-9a-f]+: SELECT status FROM runs WHERE id = \$1 FOR UPDATE/ { want=1; next }
want && /DETAIL:  parameters: \$1 = / { n++; want=0 }
END { if (tick!="") printf "tick %s host=%s : %d candidates\n", tick, host, n }
' "$SCRATCH/partD-pglog-raw.txt" | tee "$SCRATCH/partD-tick-candidates.txt"
```

**Deliverable.** The accumulated `Pending` count at D1, the kill and promotion
instants, and the candidate count of the **first tick issued by the new leader** —
which must be compared against that accumulated count, not against 50. On the
2026-07-30 run: 58 accumulated, first post-promotion tick processed exactly 50,
promotion 0.36 s ahead of the kill's own return.

**Recording — do NOT file this as a finding of its own.** On its own the
deviation from `:163` is one extra tick per 50 runs (~200 ms, bounded) on a
*queueable* backlog, and this rig never exercised a queueable backlog because the
head 50 stayed mutex-blocked through all 41 post-promotion ticks. Alone that is a
docs gap, i.e. **minor**. It carries a major only in conjunction with Parts A/B,
which is why the two are **one merged entry** resting on the single root cause
`scheduler.go:58`. Do not re-split them and do not count `scheduler.go:58` twice
in a wave tally.

## Teardown

```bash
# revert the instrument FIRST, and verify in a fresh session
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "ALTER SYSTEM RESET log_statement;"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "ALTER SYSTEM RESET log_line_prefix;"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c "SELECT pg_reload_conf();"
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_statement;"   # must print: none
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "SHOW log_line_prefix;" # must print: %m [%p]
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

> **CORRECTED AFTER EXECUTION.** The two rules below that name I1 as the primary
> invariant and I5 as "secondary and analogical" are **superseded**. As executed:
> **I1 is not claimed at all** (both probes reached exactly one terminal state,
> and I1 has no liveness bound — see the correction box at the top of this file);
> **I5 IS claimed, but only on the Part D limb**, where a real fault is injected
> and the contradicted sentence lives in the document I5 names; and Parts A/B
> alone breach **no** invariant and **no** contract, which is precisely why the
> Part D door is not filed separately. The severity argument in the first rule is
> otherwise sound and was carried into the entry. Original text follows.

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

## Execution notes — 2026-07-30 run (read before re-running)

Executed against `test/ha` at branch `plan/edge-case-w2`, `06:21:49Z – 06:49:55Z`.
`log_statement='all'` was armed and reverted **three** times (short windows on
purpose, see correction 3) and `log_line_prefix` was also `RESET` at teardown;
every revert was verified in a **fresh** session (`w2-9/gate-g3-arm.txt`,
`w2-9/partA-pglog-disarm.txt`, `w2-9/partB-pglog-disarm.txt`,
`w2-9/partD-disarm.txt`, `w2-9/teardown.txt`). Stack torn down with `down -v`
after cancelling all 64 non-terminal runs and confirming `mutex_holders` and
held `named_lock_slots` were both empty. **Sampler hygiene was captured, not
asserted:** `jobs` printed nothing and `ps -W | grep -iE "psql|curl|python"`
matched nothing, on **two** passes (before the final revert and immediately
before `down -v`) — `w2-9/teardown.txt`. **Two `FINDINGS` entries: 1 violation
(major) and 1 observation (minor)**; no branch-internal asset bug. (An earlier
draft filed **three** entries, splitting the Parts A/B door and the Part D door
into two majors. They are the same root cause — the `limit = 50` at
`scheduler.go:58` — and were merged into one; **W2-9 contributes 1 major + 1
minor to the wave tally**.) The dev stack (`docker compose ls` project
`unified-cd`) was untouched.

**The hypothesis held in full, and the threshold is 50.** Part A reproduced on
the first attempt: 36/36 poll samples `Pending`, 127 consecutive ticks whose
candidate set was set-identical to the 50 oldest `Pending` rows, and the probe
absent from every one. Part B's transition tick returned 50 candidates **with**
the probe among them. **State the instrument precisely when you write this up:**
the count at admission is the per-candidate
`SELECT status FROM runs WHERE id = $1 FOR UPDATE` tally used as a **1:1 proxy**
for the snapshot's row count — verified from `internal/store/postgres.go:482-489`
(that `FOR UPDATE` is `tryQueueRun`'s first statement after `BEGIN`, with no
earlier return path, so exactly one is issued per snapshot row). It is not read
off the snapshot statement itself, which logs no row count. A second,
cancel-free round agreed. §(1)'s arithmetic prediction was
correct; it is now a measurement.

**Six things a re-run should know.**

1. **Part D did not exist in this runbook and is now the strongest single
   result.** `docs/high-availability.md:163` — "After promotion, the new leader
   processes any accumulated Pending Runs on the next tick — no runs are lost" —
   is **false** above 50: with 58 accumulated, the first post-promotion tick
   processed exactly 50. It is also the **cheapest** reproduction in the whole
   scenario (51 `Pending` runs + one `docker compose kill`, no probe, no cancels,
   no drain) and the only part that injects a real fault, which is what puts I5
   properly in scope. **Run it first on a re-run.** Driver:
   `../edgecase/tools/w2/w2-9-partD.sh`; the executable procedure is the Part D
   section above.
   §(4)'s doc survey missed this sentence because it greps for mutex/queue
   vocabulary; grep `docs/high-availability.md` for `Pending` directly.
   **But do not file it as a finding of its own.** On its own the deviation from
   `:163` is one extra tick per 50 runs (~200 ms, bounded) on a *queueable*
   backlog — and this session never exercised a queueable backlog, because the
   head 50 stayed mutex-blocked through all 41 post-promotion ticks. Alone that
   is a docs gap, i.e. **minor**. It carries a major only in conjunction with
   the Parts A/B measurements, which is why the two are one merged entry.
   Two scoping escapes were checked and neither applies: "no runs are lost" is
   a **second conjunct joined by an em dash** (a further promise, not a
   narrowing of "any accumulated … on the next tick"), and the W2-7 scoping
   check passes — `:163` sits under §"What happens during leader absence"
   (`:157`), is about the post-promotion tick specifically, and nothing in
   `:159-162` authorises a batch size.
2. **§G5 as written is unnecessary — the main experiment supplies it for free.**
   The blocked runs *are* the ≥5-minute observation: 49 of them sat `Pending`
   for 6 m 20.9 s – 6 m 26.4 s, and by teardown 58 were `Pending` with the
   oldest at 25 m 50.2 s, with **0** `Failed` runs and **0** reaper log lines all
   session. Do not spend a separate 5-minute serial wait on it.
3. **Budget the instrument by the second, not the minute.** At 200 ms ticks with
   50 candidates the leader alone writes ~2,300 Postgres log lines/s: a 25 s
   window is **74,951 lines / 7.9 MB**, and the three captures here total 30 MB.
   Arm, capture, disarm — never leave it on across a whole part. The rate itself
   is a finding (252.0 aborted transactions/s), so capture one window
   deliberately for that number.
4. **The parser must match both pgx forms, and here *both* matter.** The phase-1
   snapshot and the `FOR UPDATE` re-check are parameterised (`LOG: execute
   stmtcache_<hash>: …` plus a `DETAIL: parameters: $1 = '<id>'` line), while
   `begin` and `rollback` are arg-less (`LOG: statement: …`). The per-tick
   candidate counter used throughout (reproduced in
   `../edgecase/tools/w2/w2-9-partD.sh` and in Part D above) keys on the `execute … FOR UPDATE` line and
   reads the *next* `DETAIL` line; it is what turns the raw log into the
   "50 candidates, probe present/absent" table that carries the finding.
   **And halve any grep count for a statement that FAILS.** A bare
   `grep -c "INSERT INTO mutex_holders"` over the Part A capture returns
   **12,700**, not 6,350, because each failing INSERT is logged **twice** —
   once as the `execute` line and once as the `STATEMENT:` echo Postgres
   attaches to the `ERROR`. The semantic count is `12,700 / 2 = 127 × 50 =`
   **6,350**. `begin`, `rollback`, the `FOR UPDATE` and the `ERROR` line each
   read directly at 6,350; only the INSERT doubles. Both
   `w2-9/partA-pglog-analysis.txt` and `w2-9/derived-numbers.txt` record the raw
   12,700, so anyone re-reading the captures will hit this.
   **Also: `w2-9/partA-claim-log.txt`'s header line is wrong.** It says
   "during 06:23:40-06:26:30", but the 273 figure it reports is its own
   "count of agent1 claim polls in the whole 8m window" and its sample rows
   start at `06:21:50.407`. Trust the 8-minute framing, not the header range;
   re-deriving 273 from the header's ~2m50s span will not reproduce it.
5. **The probe's id will appear in the statement log even when it is starved —
   from your own polling.** All 18 hits in the Part A capture were the
   controller's `GetRun`/`GetRunParent` reads serving the harness's 5 s poll.
   Do not read a bare `grep <probeID>` hit count as evidence either way; extract
   the `FOR UPDATE` parameter set specifically.
6. **Keep the holder alive for the whole experiment, or accept the churn
   deliberately.** The 600 s hog gives ~4 minutes of margin after setup — enough
   for Part A + Part B but not for a leisurely one. The *second* round exploited
   the churn instead: after the hog exited, six `edge-sideeffect` runs held
   `edge-mutex` back to back (each acquiring within **~1 s** of its
   predecessor's release — the largest gap is
   `06:35:31.700684 → 06:35:32.707` = **1.006 s**, so do not write "within
   1.0 s" as an earlier draft did; it is false by 6 ms), and the probe stayed
   starved across **seven** holders for 787.6 s,
   which demonstrates that the starvation depends on queue depth and not on any
   one holder. That is a better result than the clean single-holder run and
   costs only patience.

**One number to read with care.** The two starvation windows (252.639 s and
787.615 s) are **not** samples of a natural distribution — the first was ended by
cancelling and the second by the backlog draining. Neither bounds the wait. What
bounds it is the code, and the code does not: no reaper touches `Pending`, and
`TransitionPendingToQueued` has no exclusion set for perpetually-blocked
candidates (unlike `ListPendingRuns`). Report the measured windows and the
code-read unboundedness separately; do not write "never".

**Part C (the git-unresolved amplifier) was NOT executed** — it needs an
unresolvable `git://` AppSource fixture this wave does not have. It is recorded
as code-read only (`postgres.go:513-518`, `scheduler.go:291`), exactly as this
runbook instructed.
