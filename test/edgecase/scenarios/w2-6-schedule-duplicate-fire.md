# W2-6 — a schedule fire is two unsynchronised writes: the run row and `last_fired_at`

- **Invariants:**
  - **I1 (run accounting)** is the *literal* fit here, and unusually so — its
    second clause names this defect by name: "every API-accepted run reaches
    exactly one terminal state; **no phantom runs from duplicate
    fires/webhooks**"
    (`docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:48`). A
    second run created for one cron occurrence *is* a phantom run from a
    duplicate fire.
  - **I2 (at-most-once side effects)** — "step side effects execute at most
    once (detected via an append-only side-effect log on a shared volume …)"
    (`:49`) — is the consequence limb: each duplicate run executes the job's
    steps independently. **Disclose the instrument gap:** this scenario's
    fixture is `edge-tick` (`test/edgecase/workloads/tick.payload.json`), whose
    only observable effect is its own log lines, so I2 is evidenced at the
    *run/step-execution* level (two independent `step_reports` rows and two
    independent log streams for one occurrence), **not** with the shared-volume
    append-only log I2's parenthetical prescribes. Say so rather than implying
    the stronger instrument was used.
  - **NOT I3** — campaign I3 is *no lock leaks* (`:50`); no mutex, semaphore or
    named-lock slot is involved in any arm here. (W2-5's entry had to be
    corrected for exactly this mislabel; do not repeat it.)
  - **I5 (bounded recovery)** is cited only in Part D/E, and only for the
    *un*bounded case: "after fault injection the system returns to steady state
    within documented bounds" (`:52`), against the published bound
    `docs/resources.md:324` ("If the controller is down during a scheduled fire
    time, the fire is caught up within 1 hour after restart").
- **Stack:** plain `test/ha` — **no overlay**. The base
  `test/ha/nginx.conf` already carries the fast-failover proxy settings
  (`proxy_connect_timeout 2s`, `proxy_next_upstream`, `max_fails=1`) that
  `nginx-edge.conf` was created for, so the `oneway` overlay buys this scenario
  nothing and its `/data` mount is unused here. Every compose call is:

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

- **Workloads:** `tick.payload.json` (job `edge-tick`: one 30 s native step) and
  `schedule-every-minute.payload.json` (schedule `edge-every-minute`,
  `cron: "* * * * *"`, job `edge-tick`). **Schedules are NOT applied through
  `POST /api/v1/jobs`** — the plan, the Task 7 brief and `test/edgecase/README.md`
  all said they were, and all three were wrong. `POST /api/v1/jobs` unmarshals the
  body into `dsl.Spec` and answers **400** `invalid yaml: … field cron not found in
  type dsl.Spec`; the schedule endpoint is **`POST /api/v1/schedules`**, which
  answers 200 with the schedule JSON. `README.md` is corrected on this branch.
- **Instrumentation:** (1) the continuous divergence sampler of §"The detector";
  (2) Postgres `log_statement='all'`, the campaign's established per-tick DB
  instrument (W2-1 → W2-4) — here it is not optional, because the *bind
  parameter* of `UPDATE schedules SET last_fired_at = $1` is the only surface
  anywhere in the system that names the occurrence a fire was for; (3)
  `"scheduler became leader"` (`internal/controller/scheduler.go:55`), the only
  leadership log line in the system and the primary attribution instrument;
  (4) `pg_locks` for the scheduler key, which per W2-1 is the *one* advisory key
  a point-in-time census can see reliably, because the scheduler is the one job
  that holds its lock across ticks (`scheduler.go:30,45-56`).

## Verified mechanism (read before running; do not re-derive)

> **CORRECTED AFTER THE 2026-07-30 RUN — five claims below are wrong or
> incomplete, and the mistakes in the first and the fifth are load-bearing. Read
> the "Execution notes" at the end of this file before re-running.**
>
> 1. **§(1)'s "the difference between `last_fired_at` and `created_at` is the
>    check phase, in (0, 60)" is FALSE for a 1-minute cron.** A schedule with
>    `last_fired_at IS NULL` fires the first occurrence after `now-1h`
>    (`scheduler.go:92-97`), and a 1-minute schedule can never drain that
>    backlog, so the steady-state difference is **~3587-3592 s**, measured on two
>    independent incarnations. This is itself a major finding
>    (`FINDINGS.md`, W2-6 "born ~59.9 minutes behind").
> 2. **§(3)'s three failover cases collapse in the state actually observed.**
>    Because the schedule is permanently in backlog, *every* check has an
>    occurrence due, so *every* promotion fires immediately — measured **8** times
>    as pairs of fires **123-372 ms** apart carrying **different** occurrence
>    binds (whenever the promotion landed *after* that minute's check had already
>    run; the other 12 promotions merely pulled the check a few hundred ms
>    earlier). That makes an ordinary failover an *extra catch-up* fire, never a
>    duplicate, which is the control §(3) was written to provide — but the
>    reasoning that gets there is different from what §(3) says. **And the extra
>    fire is not free bookkeeping: each one cut ~59.7 s off the schedule's lag
>    (total −478.156 s over Arm A), so 20 kills left the schedule *healthier* than
>    they found it.** Any re-run must expect leader churn to mask the drift.
> 3. **The detector's "steady state: `d` ≈ the check phase" reading is wrong for
>    the same reason** (it is ~3587), and the "`d` jumps by a full cron period"
>    fingerprint is **transient — it lasted 60.36 s and was then erased** by the
>    silent-advance branch. Read `n` (the fire count) as the durable signal.
> 4. **Part E's DB backdating was not needed and was not performed.** Part D
>    reached the `:197-201` silent-skip branch by pure fault injection.
> 5. **§(1)'s table said the `:194` `UPDATE` runs "on another pool connection".
>    It does not, and this is the load-bearing correction — it has been fixed in
>    the table below.** In all 48 fires captured live under `log_statement='all'`
>    the `INSERT INTO runs` and the `UPDATE schedules SET last_fired_at` were
>    logged under the **same** Postgres backend pid (the pid changes *between*
>    fires, as pgxpool hands out different connections, never *within* one). The
>    defect is **two autocommit statements with no enclosing `Begin`**, not two
>    connections — and the difference matters because the wrong phrasing sends a
>    fixer to pool configuration instead of to the missing transaction.

### (1) The window, and what `last_fired_at` actually stores

`checkAndFireSchedules` (`internal/controller/scheduler.go:86-204`) contains no
`Begin`/`Commit` anywhere. For a due occurrence it does, in order:

| # | Line | Call | Failure handling |
|---|---|---|---|
| 1 | `:189` | `st.CreateRun(...)` → `INSERT INTO runs … RETURNING id` on a pooled connection (`internal/store/postgres.go:219-263`) | error → `continue`, `last_fired_at` untouched, retried next check |
| 2 | `:194` | `st.UpdateScheduleLastFiredAt(ctx, sc.Name, next)` → `UPDATE schedules SET last_fired_at=$1, updated_at=NOW() WHERE name=$2` (`postgres.go:2159-2166`) — **measured live: the same Postgres backend pid as row 1, in all 48 fires.** The two statements are separate *autocommit transactions*, not separate connections; there is no `Begin` anywhere in `checkAndFireSchedules` | **error → `slog.Warn` at `:195` and nothing else. No retry, no compensation, no metric.** |

**The value written is `next` — the cron occurrence — not the wall-clock instant
of the fire.** That single fact governs every measurement below: for a
`* * * * *` schedule, `last_fired_at` is always exactly on a `:00`-second minute
boundary, while the run's `created_at` is the instant the leader's minute-cadence
gate happened to fire. The difference between them is not error — it is the
**check phase**, and it is stable for the lifetime of one leadership epoch.

### (2) Why a leadership change re-checks *immediately*, and why that matters

`lastScheduleCheck` is a **local variable of `RunScheduler`** (`scheduler.go:31`),
zero-valued at process start, and the gate is
`if t.Sub(lastScheduleCheck) >= time.Minute` (`:71`). A freshly promoted leader
therefore has `lastScheduleCheck == time.Time{}`, the subtraction is enormous,
and **it runs `checkAndFireSchedules` on its very first tick as leader** — within
one 200 ms tick of acquiring the lock. There is no persisted or shared
check-cadence state anywhere.

Consequences, all needed to interpret the arms:

- **The duplicate is immediate, not delayed by a minute.** If the outgoing leader
  created a run and did not get to `:194`, the incoming leader re-computes
  `next` from the stale `last_fired_at`, finds the *same* occurrence still due,
  and fires it again seconds later.
- **The check phase is reset by every failover**, so "two runs in the same clock
  minute" is **not** a sound duplicate test — a failover legitimately produces
  two runs close together *for different occurrences* (see (3)). The sound tests
  are (a) two `UPDATE schedules … last_fired_at = $1` binds carrying the **same**
  value, and (b) total fires exceeding elapsed cron occurrences.
- Nothing anywhere checks whether a run already exists for an occurrence:
  `triggered_by` is `"schedule:"+sc.Name` (`:189`) with no timestamp, and there
  is no unique index involving it (`internal/store/migrations/001_init.up.sql:213-240`).

### (3) The failover cases that are *not* duplicates (needed for the control)

With `base = last_fired_at = X` and `sched.Next(base)` strictly after `base`
(`internal/dsl/schedule_parse.go:54-60`, robfig `sched.Next(after)`), a leader
whose phase is `p` fires occurrence `X` at `X+p` and would next fire `X+60` at
`X+60+p`. Kill it at time `T`:

- `T ∈ (X+p, X+60)` → new leader computes `next = X+60 > T` → **no fire**, and it
  will fire `X+60` at `T+60`… i.e. the schedule's phase simply moves.
- `T ∈ [X+60, X+60+p)` → new leader fires `X+60` **early**, at `T` instead of
  `X+60+p`. Correct catch-up, not a duplicate. This is the common case for
  arbitrary kills and is why Arm A must be aimed, not sprayed.
- Kill inside `:189`→`:194` → new leader fires `X` **again**. This is the defect.

### (4) The silent-skip branch, already filed

`:197-201` (`default:`) advances `last_fired_at` to a `next` older than
`now-1h` with **no run created and no log line of any kind**. That is the W0-2
major already in `FINDINGS.md:88` ("Catch-up window boundary: an occurrence
missed by one cron interval (5 min) past the 1h window is silently and
permanently dropped"). **Part E demonstrates the path live and cross-references
that entry; it must not re-file it.**

### (5) The drain-rate corollary (relevant to Part D, and W0-2 tested only the easy case)

The backlog drains at **exactly one occurrence per `checkAndFireSchedules`
call** — established by W0-2's probe (`FINDINGS.md:138-149`) on a `*/5`
schedule, where a 30-minute backlog drained in ~6 real minutes because one call
per minute retires one 5-minute occurrence. **For a `* * * * *` schedule the
drain rate equals the accumulation rate**, so a backlog of *k* minutes is
permanent: every check retires exactly the one occurrence that the passing
minute just added. W0-2's own numbers make this a corollary rather than a new
mechanism — cross-reference it, and be explicit that the 1-per-call rate is
W0-2's measurement and the "never drains for period ≤ 1 min" consequence is what
this scenario adds and must itself be measured, not asserted.

### (6) What the docs promise (search before filing)

- **`docs/resources.md:324`** — "If the controller is down during a scheduled
  fire time, the fire is caught up within 1 hour after restart." A published
  *recovery* bound; the natural citation for Part D/E if divergence is shown
  not to close.
- **`docs/high-availability.md:159,163`** — "Only **Pending→Queued transitions
  and schedule fires are paused** while there is no leader" and "After
  promotion, the new leader processes any accumulated Pending Runs on the next
  tick — **no runs are lost**." Both are **loss**-direction statements. Neither
  promises the *converse* (no run is created twice), so quoting them as the
  duplicate-fire contract is a scoping error — the duplicate rests on **I1**.
- **`docs/operations.md:77`** and **`docs/troubleshooting.md:801`** — a reaped run
  is "never re-queued, since re-running partially-executed steps can duplicate
  side effects". Corroboration of *intent* (the design treats duplicated side
  effects as a harm to be avoided), not a contract about schedules.
- **`docs/high-availability.md:81`** — "Only the leader transitions
  Pending→Queued and fires schedules." True and not contradicted: the two
  duplicate fires come from two *sequential* leaders, not concurrent ones. Say
  so, so the entry is not misread as a split-brain claim (that is W0-1's).
- `scheduler.go:79-85`'s own doc comment enumerates the four "do not update
  `last_fired_at`" cases and is a **precise description of the code**, so it
  cannot be cited as a violated contract (campaign rule, `FINDINGS.md:479`):
  an unexported function's own comment is not published documentation. Note that
  the comment is *silent* about the `:194` failure — the one case where the
  run **is** created and `last_fired_at` is **not** advanced.

## The detector (runs continuously, across every arm)

Cheap, one row every 2 s, and it is the actual defect detector. `d` is the
divergence between what the schedule *thinks* it last fired and the newest run
it actually produced:

```bash
SCRATCH=<scratchpad>/w2-6 ; mkdir -p "$SCRATCH"
docker compose $COMPOSE_FILES exec -T postgres sh -c '
  for i in $(seq 1 2400); do
    psql -U unified -tAc "
      SELECT to_char(NOW(),'\''HH24:MI:SS.MS'\'') AS t,
             to_char(s.last_fired_at,'\''HH24:MI:SS'\'') AS lfa,
             to_char(r.created_at,'\''HH24:MI:SS.MS'\'') AS newest_run,
             round(EXTRACT(EPOCH FROM (r.created_at - s.last_fired_at))::numeric,3) AS d,
             (SELECT count(*) FROM runs WHERE triggered_by='\''schedule:edge-every-minute'\'') AS n
      FROM schedules s
      LEFT JOIN (SELECT created_at FROM runs WHERE triggered_by='\''schedule:edge-every-minute'\''
                 ORDER BY created_at DESC LIMIT 1) r ON true
      WHERE s.name='\''edge-every-minute'\'';"
    sleep 2
  done' > "$SCRATCH/detector.txt" &
```

Reading it:

- **Steady state:** `d` ≈ the check phase, constant within a leadership epoch,
  in `(0, 60)`. `n` increments by exactly 1 per minute.
- **Fingerprint of the `:189`→`:194` window:** `d` jumps by a **full cron period
  (≈60 s)** — a run exists for an occurrence `last_fired_at` does not know about.
- **Fingerprint of Part D:** `d` grows by ≈60 s *per minute*, monotonically,
  without bound.
- **Fingerprint of Part E:** `d` goes sharply negative (`last_fired_at` marches
  forward while `n` does not move at all).

Per W2-5's execution note, **the sampler must not also perform injection or
teardown**: long-lived `docker compose exec` pipelines have died silently
mid-run in this campaign. Every row carries `NOW()`, so a gap is visible; check
the file's tail before recording anything and restart the sampler if it stalled.

### Leader identification (needed by Arm A and Part C)

```bash
# 0x65786364 = 1702388580 (decimal). Per W2-1 this is the ONE advisory key a point-in-time
# census can see, because it is the one lock held across ticks.
psql "SELECT a.client_addr, a.pid, to_char(a.backend_start,'HH24:MI:SS.MS')
      FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid
      WHERE l.locktype='advisory' AND l.objid=1702388580;"
for c in controller1 controller2 controller3; do
  printf '%s %s\n' "$c" "$(docker inspect unified-cd-ha-$c-1 \
    --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')"
done
# Cross-check against the log line, which is authoritative for the *instant*:
docker compose $COMPOSE_FILES logs -t controller1 controller2 controller3 \
  | grep "scheduler became leader"
```

Both instruments are recorded for every kill: `pg_locks` says *who* holds it now,
`"scheduler became leader"` says *when* the holder acquired it — and the
acquisition instant is what predicts the check instants (`T0 + 60k`).

## Baseline gate

Confirm all of these before recording anything. If any fails, STOP and report
BLOCKED with the evidence.

```bash
curl -s -o /dev/null -w 'readyz=%{http_code}\n' localhost:18080/readyz
docker compose $COMPOSE_FILES ps --format '{{.Service}} {{.State}}'
curl -fsS localhost:18080/api/v1/agents -H "Authorization: Bearer ha-admin-token" | tee "$SCRATCH/agents.json"

# G1. Statement logging on, with host attribution. log_parameter_max_length is
#     -1 on postgres:16-alpine (W2-1), so DETAIL parameter lines come free.
#     CORRECTED BY W2-7: one -c PER `ALTER SYSTEM`. Two of them in a single -c
#     form one implicit transaction and Postgres refuses with `ALTER SYSTEM
#     cannot run inside a transaction block`; pg_reload_conf() then still
#     returns t and a NEW session still reports `none`, so the form below fails
#     silently and looks exactly like the instrument working.
docker compose $COMPOSE_FILES exec -T postgres psql -U unified \
  -c "ALTER SYSTEM SET log_statement='all'" \
  -c "ALTER SYSTEM SET log_line_prefix='%m [%p] h=%h '" \
  -c "SELECT pg_reload_conf()"
psql "SHOW log_statement;" ; psql "SHOW log_parameter_max_length;"   # expect all / -1

# G2. Exactly one scheduler-lock holder, and identify it.
psql "SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid=1702388580;"  # expect 1

# G3. Apply the job, then the schedule. Schedule fires start immediately:
#     last_fired_at is NULL so base = now-1h and the newest past occurrence is due.
curl -fsS -X POST localhost:18080/api/v1/jobs \
  -H "Authorization: Bearer ha-admin-token" -H 'Content-Type: application/json' \
  --data-binary @../edgecase/workloads/tick.payload.json -o /dev/null -w "tick=%{http_code}\n"
# NOTE the DIFFERENT path. POST /api/v1/jobs returns 400 for a kind: Schedule.
curl -fsS -X POST localhost:18080/api/v1/schedules \
  -H "Authorization: Bearer ha-admin-token" -H 'Content-Type: application/json' \
  --data-binary @../edgecase/workloads/schedule-every-minute.payload.json \
  -w "\nschedule=%{http_code}\n"                                # expect 200 x2
psql "SELECT name, cron, job_name, last_fired_at FROM schedules;"

# G4. Host <-> DB clock skew (kill instants are host clock; everything else is DB clock).
psql "SELECT NOW();" ; date -u +%FT%T.%3NZ
```

**Gate G5 — the detector's normal range must be established before any
injection.** Start the sampler and let it run ≥ 3 full minutes uninjected. It
must show `n` incrementing by exactly 1 per minute and `d` stable to within a
second or so. If `d` is not stable, the phase model in §(1) is wrong and the
whole detector is invalid — STOP and report that instead.

## Part A — control: uninjected steady state, and the width of the window being raced

No injection. ≥ 4 minutes. Deliverables:

1. **`last_fired_at` semantics, live.** Confirm every `last_fired_at` sample is
   exactly on a minute boundary (`:00.000`) while `created_at` is not — i.e. the
   column stores the occurrence, not the fire.
2. **The check phase and its stability**, from the detector's `d` column.
3. **One run per occurrence.** `n` over the window vs elapsed minutes.
4. **The width of the `:189`→`:194` window, MEASURED** — this is what makes Arm A
   reportable rather than hand-waved. From the Postgres log, pair each
   `INSERT INTO runs` with the `UPDATE schedules SET last_fired_at` that follows
   it on the same host, and take the difference of the `%m` timestamps:

   ```bash
   docker compose $COMPOSE_FILES logs postgres > "$SCRATCH/pg-partA.log"
   grep -nE "INSERT INTO runs|UPDATE schedules SET last_fired_at|FROM schedules" "$SCRATCH/pg-partA.log" \
     | tee "$SCRATCH/partA-window.txt"
   ```

   Report the min/median/max over as many fires as the window contains, and say
   how many fires that is. The plan's "single-digit milliseconds" is *inferred*
   (`docs/superpowers/plans/2026-07-30-edge-case-campaign-w2.md:74` and the
   Task 7 brief); this replaces it with a measurement or contradicts it.
5. **The occurrence bind values.** Confirm `DETAIL: parameters: $1 = '<ts>'` on
   each `UPDATE schedules` is a strictly increasing sequence of minute
   boundaries, 60 s apart — the baseline that makes a *repeated* bind value in
   Part C unambiguous.

## Part B — Arm A: race the `:189`→`:194` window with `kill-hard`

**Cap: 20 attempts. Report the count and the outcome of every attempt whether or
not it hits.** Per the brief and per W2-3's Arm D precedent (0/10, accepted), a
miss is a reportable result — but it must be filed as "not reproduced live" with
the code-read argument, never as "the window is unreachable".

**Aiming, not spraying.** Per §(3) most kill instants cannot produce a duplicate
at all, so each attempt must be aimed at a fire:

- The incoming leader fires **immediately on promotion** whenever an occurrence
  is due (§(2)). So kill the current leader at a wall-clock instant just past a
  minute boundary at which the *outgoing* leader has not yet fired that
  occurrence — i.e. when `NOW() - last_fired_at > 60 s`. The detector's `d`/`lfa`
  columns tell you this directly.
- That converts each attempt into "the incoming leader's fire happens within
  ~200 ms–2 s of my kill", and the attempt is then to kill *that* leader inside
  its own `:189`→`:194` window.
- **Phase-locking, per W2-4's rule:** the fire instant is predicted by the
  *promotion* instant (`"scheduler became leader"`), and thereafter by
  `T0 + 60k`. Aim at those, not at nominal cron boundaries. Record the predicted
  instant, the actual kill instant, and their difference for every attempt.

```bash
# Repeat per attempt; ATTEMPT is the trial number.
LEADER=<from the leader-identification block>
date -u +%FT%T.%3NZ                            | tee -a "$SCRATCH/armA-kills.txt"
sh ../edgecase/tools/inject.sh kill-hard "$LEADER" | tee -a "$SCRATCH/armA-kills.txt"
psql "SELECT to_char(NOW(),'HH24:MI:SS.MS'), name, to_char(last_fired_at,'HH24:MI:SS') FROM schedules;" \
  | tee -a "$SCRATCH/armA-kills.txt"
# restore before the next attempt so there are always >=2 candidates
docker compose $COMPOSE_FILES up -d controller1 controller2 controller3
```

Only **two** controllers may be killed before restoring: with one replica left,
killing it leaves no leader at all and the arm measures nothing.

Verdict per attempt — from the Postgres log, not from run counts:

```bash
docker compose $COMPOSE_FILES logs postgres > "$SCRATCH/pg-armA.log"
grep -E "UPDATE schedules SET last_fired_at|\\\$1 = " "$SCRATCH/pg-armA.log" | tee "$SCRATCH/armA-binds.txt"
```

A hit is **two `INSERT INTO runs` whose associated `UPDATE schedules` bind
`$1` is the same occurrence** (or an `INSERT` with no following `UPDATE` at all,
followed by a re-fire of the same occurrence). Also record, for the whole arm,
the total fires vs the elapsed occurrence count.

## Part C — Arm B1: the same defect, deterministically, by holding the second write open

**This is the arm that is expected to produce the result**, and it is the direct
descendant of W2-5's "convert a sub-10 ms race into a deterministic arm"
technique (`FINDINGS.md:885` notes). Instead of racing the gap between the two
writes, **make the second write block** and kill the leader while it is blocked.

```bash
# C1. Take an exclusive row lock on the schedule and hold it. Run BACKGROUNDED:
#     it occupies the session for the duration.
docker compose $COMPOSE_FILES exec -T postgres psql -U unified <<'SQL'
BEGIN;
SELECT NOW() AS lock_taken, name, last_fired_at FROM schedules
  WHERE name='edge-every-minute' FOR UPDATE;
SELECT pg_sleep(150);
ROLLBACK;
SQL
```

```bash
# C2. Confirm the leader is blocked ON that row, from Postgres itself, before
#     killing anything. This is the arm's precondition and must be evidenced,
#     not assumed.
psql "SELECT a.pid, a.client_addr, a.wait_event_type, a.wait_event, a.state,
             to_char(a.query_start,'HH24:MI:SS.MS'), left(a.query,60)
      FROM pg_stat_activity a WHERE a.query ILIKE '%schedules SET last_fired_at%';"
# C3. The run for this occurrence already exists while last_fired_at does not
#     know it: this is the window, held open.
psql "SELECT to_char(NOW(),'HH24:MI:SS.MS'), to_char(last_fired_at,'HH24:MI:SS') FROM schedules WHERE name='edge-every-minute';"
psql "SELECT id, status, to_char(created_at,'HH24:MI:SS.MS') FROM runs
      WHERE triggered_by='schedule:edge-every-minute' ORDER BY created_at DESC LIMIT 3;"
# C4. Kill the blocked leader. The successor re-checks within one 200ms tick.
date -u +%FT%T.%3NZ ; sh ../edgecase/tools/inject.sh kill-hard "$LEADER"
```

Then, after the lock's `pg_sleep` expires and both updates drain:

```bash
docker compose $COMPOSE_FILES logs postgres > "$SCRATCH/pg-partC.log"
grep -nE "INSERT INTO runs|UPDATE schedules SET last_fired_at|parameters: \\\$1" "$SCRATCH/pg-partC.log" \
  | tee "$SCRATCH/partC-binds.txt"
psql "SELECT id, status, to_char(created_at,'HH24:MI:SS.MS'), claimed_by
      FROM runs WHERE triggered_by='schedule:edge-every-minute' ORDER BY created_at;" \
  | tee "$SCRATCH/partC-runs.txt"
psql "SELECT run_id, step_index, step_name, status FROM step_reports
      WHERE run_id IN ('<R1>','<R2>') ORDER BY run_id, step_index;" | tee "$SCRATCH/partC-steps.txt"
docker compose $COMPOSE_FILES up -d controller1 controller2 controller3
```

**Deliverables:** the two run ids and their `created_at`; the **identical `$1`
bind** on the two `UPDATE schedules` statements, which is the proof they were
the same occurrence; both runs' terminal status and step rows (the I2 limb: the
step executed twice for one occurrence); and the interval between the two
fires.

**Two costs of this substitution, to be stated in the entry:**

1. **A held row lock is a stand-in**, not a reproduction of a specific
   production incident. What it reproduces faithfully is the *state* the
   `:189`→`:194` window produces — run committed, `last_fired_at` not advanced —
   which is also reachable from a `kill -9`, an OOM, a pod eviction, a pool
   connection reset, a failover of the DB, or a `statement_timeout`. What it does
   **not** establish is how often that happens in production.
2. **The blocked write stalls the entire scheduler goroutine**, because
   `checkAndFireSchedules` is called synchronously from the tick loop
   (`scheduler.go:72`) and the `Exec` carries the goroutine's own long-lived ctx
   with no timeout. So while the lock is held, `TransitionPendingToQueued`
   (`:58`) does not run either and **no** Pending run in the cluster is
   promoted. Measure that side effect (it is a finding in its own right —
   one locked row denies scheduling cluster-wide) rather than treating it as
   noise.

## Part D — Arm B2: the un-retried update failure, with no kill at all

The brief's Arm B in its deterministic form. `:195` logs a Warn and returns; the
fire is never compensated. Make the write fail — not block — and observe what the
schedule does for several minutes.

```bash
# D1. Arm: a BEFORE UPDATE trigger scoped to this one schedule row.
docker compose $COMPOSE_FILES exec -T postgres psql -U unified <<'SQL'
CREATE OR REPLACE FUNCTION edge_block_sched_update() RETURNS trigger
  LANGUAGE plpgsql AS $$ BEGIN
    RAISE EXCEPTION 'w2-6 injection: schedules UPDATE refused';
  END $$;
CREATE TRIGGER edge_block_sched_update BEFORE UPDATE ON schedules
  FOR EACH ROW WHEN (NEW.name = 'edge-every-minute')
  EXECUTE FUNCTION edge_block_sched_update();
SQL
date -u +%FT%T.%3NZ | tee "$SCRATCH/partD-arm.txt"
```

Hold for ≥ 3 minutes, then:

```bash
# D2. The Warn line, once per check, on the leader only.
docker compose $COMPOSE_FILES logs -t controller1 controller2 controller3 \
  | grep -E "failed to update last_fired_at|scheduler became leader" | tee "$SCRATCH/partD-warns.txt"
# D3. Disarm and watch whether it self-corrects.
docker compose $COMPOSE_FILES exec -T postgres psql -U unified -c \
  "DROP TRIGGER edge_block_sched_update ON schedules; DROP FUNCTION edge_block_sched_update();"
date -u +%FT%T.%3NZ | tee "$SCRATCH/partD-disarm.txt"
```

Then let the detector run ≥ 3 more minutes and read `d` off it.

**Deliverables:** how many runs were created while armed and which occurrence
each was attributed to (all the same one — read the repeated `$1` bind);
whether `d` grew by ~60 s per minute; and, after disarming, **whether `d`
returns to the Part A baseline or stays at its accumulated value**. The brief
makes the severity turn on exactly that: self-correcting → minor, permanent →
major. Predicted by §(5) to be permanent for a 1-minute cron; **measure it,
do not assert it**, and if it does drain, that contradicts §(5) and is itself
the finding.

## Part E — Arm C: the silent-skip path (a DEMONSTRATION of a code path)

**State plainly in the entry: this arm mutates DB state directly rather than
injecting a fault, so it demonstrates a code path and is not a
naturally-occurring observation.** Precedent: W2-3 and W2-4 both did this and
both labelled it.

```bash
# E1. Backdate past the 1h catch-up window.
psql "UPDATE schedules SET last_fired_at = NOW() - interval '2 hours' WHERE name='edge-every-minute';"
date -u +%FT%T.%3NZ ; psql "SELECT to_char(NOW(),'HH24:MI:SS'), to_char(last_fired_at,'HH24:MI:SS') FROM schedules WHERE name='edge-every-minute';"
N_BEFORE=$(psql "SELECT count(*) FROM runs WHERE triggered_by='schedule:edge-every-minute';")
```

Wait through ≥ 2 checks (~2 min), then:

```bash
psql "SELECT to_char(NOW(),'HH24:MI:SS'), to_char(last_fired_at,'HH24:MI:SS') FROM schedules WHERE name='edge-every-minute';"
psql "SELECT count(*) FROM runs WHERE triggered_by='schedule:edge-every-minute';"   # expect == N_BEFORE
docker compose $COMPOSE_FILES logs --since 5m controller1 controller2 controller3 \
  | grep -icE "schedule|last_fired" | tee "$SCRATCH/partE-logs.txt"
grep -E "UPDATE schedules SET last_fired_at" "$SCRATCH/pg-partE.log" | tee "$SCRATCH/partE-binds.txt"
```

**Deliverables:** `last_fired_at` advances by exactly one cron period per check
with **zero** runs created and **zero** log lines; therefore the walk-back rate
is 1 occurrence/minute and a 2-hour gap costs ~60 minutes of silent walking plus
~60 permanently dropped occurrences — the *rate* is measured from 2-3 checks and
the ~60-minute total is **derived, and must be labelled derived**. Cross-reference
`FINDINGS.md:88`; do not re-file.

## Teardown

```bash
# Revert the Postgres instrumentation and SAY that you did (campaign rule).
# CORRECTED BY W2-7: one -c per ALTER SYSTEM — see gate G1.
docker compose $COMPOSE_FILES exec -T postgres psql -U unified \
  -c "ALTER SYSTEM RESET log_statement" \
  -c "ALTER SYSTEM RESET log_line_prefix" \
  -c "SELECT pg_reload_conf()" -c "SHOW log_statement"
# Drop the Part D injection if any arm exited early.
docker compose $COMPOSE_FILES exec -T postgres psql -U unified \
  -c "DROP TRIGGER IF EXISTS edge_block_sched_update ON schedules;" \
  -c "DROP FUNCTION IF EXISTS edge_block_sched_update();"
docker compose $COMPOSE_FILES logs postgres > "$SCRATCH/pg-full.log"
docker compose $COMPOSE_FILES logs -t controller1 controller2 controller3 > "$SCRATCH/controllers-full.log"
docker compose $COMPOSE_FILES down -v
docker compose $COMPOSE_FILES ps -a
```

`down -v` removes the volume, so the `ALTER SYSTEM` revert is belt-and-braces —
do it anyway, and record that it was done, because the campaign rule is about
leaving no instrumentation behind rather than about this particular volume.

## Recording (severity guidance)

- **Two runs for one cron occurrence = major (I1's "no phantom runs from
  duplicate fires" is literal, not stretched).** Rest it on I1, add I2 for the
  duplicated step execution *with the instrument gap disclosed*, and do **not**
  claim `docs/high-availability.md:159/163` as the contract — those are
  loss-direction statements (see §(6)).
- **`last_fired_at` diverging from the newest run = major or minor by whether it
  self-corrects** (the brief's rule). Part D answers this; the published bound to
  cite if it does not close is `docs/resources.md:324` under **I5**.
- **The scheduler-wide stall from one locked row (Part C cost 2) is its own
  entry** — an observation at minimum: a single row lock on `schedules` denies
  `Pending→Queued` promotion for the whole cluster for as long as it is held,
  because the schedule check is inline on the tick loop and the write carries no
  timeout.
- **The Part E silent skip is W0-2's already-filed major** (`FINDINGS.md:88`).
  Cross-reference. What may be *added* is the measured walk-back rate, if W0-2
  did not measure it live (it used a Go probe, not the live stack).
- **Arm A's count is mandatory output** whether it hits or misses, alongside the
  measured window width from Part A.4. A miss is filed as "not reproduced live"
  with `scheduler.go:189-196` cited — not as evidence of safety.
- Every entry title that is an observation must contain the word
  "observation" (`FINDINGS.md:481`).
- Uncaptured live observations carry
  `(observed live, raw output not captured to scratchpad)`.

## Execution notes (added after the 2026-07-30 run — read before re-running)

- **Outcome: both results landed, and they turned out to be two sides of one
  threshold.** Part C produced the duplicate (two runs bound to occurrence
  `2026-07-30 00:22:00+00`, 2.030 s apart, both `Succeeded`, 30 log rows each);
  Part D produced the opposite outcome from the *same* fault (one refused
  `last_fired_at` write) because the next check arrived 60.005 s later against
  only 8.418 s of headroom, so the occurrence had fallen out of the 1-hour
  catch-up window and was silently dropped instead of re-fired — after which the
  schedule never fired again (8 consecutive checks, 0 runs, 0 log lines). Full
  numbers in `FINDINGS.md` (two W2-6 majors and two W2-6 observations).
- **The stack needs no overlay, and the base `test/ha/nginx.conf` was
  sufficient.** **21** `SIGKILL`s of controllers over 48 minutes (20 in Arm A plus
  Part C's one — note that the session's *promotion* count is 22, one more, because
  the initial election was not preceded by a kill) produced no API errors at all;
  the base config already carries `proxy_next_upstream`,
  `max_fails=1` and a 2 s connect timeout. The `oneway` overlay was started
  once by mistake and then removed with a plain `up -d`, which cleanly recreated
  only nginx.
- **The decimal form of the scheduler advisory key is `1702388580`, not
  `1702389604`.** The first draft of gate G2 had the wrong conversion and read
  **0** holders, which looks exactly like "the scheduler is dead". Always
  cross-check with `SELECT objid FROM pg_locks WHERE locktype='advisory'`.
- **`client_addr` is `inet`, so it renders as `172.20.0.3/32`.** A `case`
  statement matching bare IPs silently falls through to "no leader". Strip with
  `${ip%%/*}`.
- **`docker compose logs -t <service>` prefixes the service name even for a
  single service** — `awk '{print $1}'` yields `controller2-1`. Use
  `--no-log-prefix`. And `--tail 500` is not enough to reach a promotion line on
  a quiet controller (531 total lines, the promotion was line ~1); use a large
  `--tail` or none.
- **Aiming Arm A at `promotion + 60k` works to ~±0.5 s and that is not close
  enough.** 20 attempts, kills issued between −0.515 s and +0.037 s of the
  predicted check, 0 hits against a measured 1-3 ms window. Do not spend more
  attempts on it: widen the window instead (Part C), which is deterministic and
  took one trial.
- **A held `SELECT … FOR UPDATE` on the schedule row is the right widening
  tool, and it does three useful things at once**: it holds the `:189`→`:194`
  window open for as long as you like, it is observable from inside
  (`pg_stat_activity.wait_event_type = 'Lock'` for the leader's own backend), and
  it doubles as the scheduler-stall probe. It writes nothing, so it is not a
  state mutation.
- **Order the stall probe BEFORE the kill, not after.** The first attempt
  triggered the manual run 0.03 s before the `SIGKILL`, so the successor's
  promotion queued it immediately and the stall measured nothing. Part C2 re-ran
  it with no kill at all and measured 51.335 s of `Pending` on an unrelated run.
- **Recreating a schedule requires DELETE + POST.** `UpsertSchedule`
  (`internal/store/postgres.go:2108-2122`) preserves `last_fired_at` on conflict,
  so re-applying does not reset it.
- **A `BEFORE UPDATE … RAISE EXCEPTION` trigger is the way to fail one write;
  `REVOKE UPDATE` is not.** `unified` is the `POSTGRES_USER` superuser and
  bypasses privilege checks entirely.
- **The host-side detector loop (`docker compose exec` per sample, `sleep 1.5` →
  ~2.09 s effective cadence once exec latency is counted) survived 48 minutes and
  21 controller kills without a gap**, which is a better record than the
  in-container `for` loops W2-5 lost — **1,365 data rows over 2843.96 s, max gap
  4.18 s, nothing above 5 s.** Budget ~2.1 s per sample, not 1.5 s, when sizing a
  window. It must be stopped explicitly before teardown or it writes
  connection errors into its own capture. **On this run that was not done**: the
  loop outlived `down -v` and kept appending
  `service "postgres" is not running` to `w2-6/detector.txt` indefinitely — 847
  such rows within half an hour of teardown, and still growing when the capture
  was audited. No evidence was damaged (every error row post-dates the last data
  sample at `01:29:52.407`, and none is interleaved), but the file's tail is
  garbage and it grows without bound. Kill the loop's PID explicitly; do not rely
  on teardown to stop it.
- **Budget ~50 minutes of wall time** on a warm stack: gate ~2 min, Part A
  ~6 min, Part B 20 attempts ~20 min (one per minute), Part C ~2 min, Part C2
  ~2 min, Part D ~11 min (recreate + 2 fires + 6 checks), census and teardown
  ~2 min. Postgres statement logging costs ~114 MB for that window; gzip the
  captures before copying them to the evidence root.
- **Instrumentation was reverted and verified in a fresh session**:
  `ALTER SYSTEM RESET log_statement; ALTER SYSTEM RESET log_line_prefix;
  SELECT pg_reload_conf();` then `SHOW log_statement` → `none` and
  `SHOW log_line_prefix` → `%m [%p] ` (`w2-6/teardown.txt`). Note that `SHOW` in
  the *same* psql session that issued the reload still returned `all`; only a new
  session showed the reverted value. The Part D trigger and function were dropped
  and confirmed absent from `pg_trigger`/`pg_proc` before teardown, and the stack
  was torn down with `down -v`.
