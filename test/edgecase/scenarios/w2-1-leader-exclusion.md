# W2-1 — leader exclusion across every background job

- **Invariants:** I1 (run accounting) is the pass/fail limb: a background job
  that fails, reaps or fires on two replicas at once produces duplicated
  state transitions, so "exactly one replica acts per tick" is the property
  under test. Everything else here is a documentation/arbitration question.
- **Stack:** plain `test/ha` compose, **no overlay**. Every compose call is:

  ```bash
  cd test/ha
  docker compose -f docker-compose.ha.yaml up -d --build
  ```

- **Workload:** none. This scenario is deliberately run against an **idle**
  stack — the point is that leader exclusion must hold when there is no work,
  which is exactly the case the system is silent about.
- **Instrumentation:** Postgres statement logging, enabled **at runtime**
  through `ALTER SYSTEM` + `pg_reload_conf()` inside the `postgres` container.
  This changes a *database server* setting, not product code and not
  `test/ha/`, and it is reverted in Teardown. It is the only way to observe
  the ten per-tick jobs, whose locks are held for milliseconds (see below).

## Verified API/mechanism (do not re-derive)

Read this before running. **Two premises in the W2 plan's Verified-facts block
do not survive contact with the code, and the runbook is written to test them
rather than to assume them.** Both are recorded here rather than silently
worked around.

### (1) A point-in-time `pg_locks` census cannot show ten of the eleven keys

The plan directs a `pg_locks` census at three sampling points ~30s apart and
says to "expect exactly one holder per key for each of the eleven keys that is
actually wired". That expectation contradicts the plan's own next bullet:
**only `RunScheduler` holds its lock across ticks** (`internal/controller/scheduler.go:22,45-56`
— acquired once, kept for the goroutine's lifetime). Every other job calls
`AcquireAdvisoryLock` at the top of a `...Once`/`...AsLeader` helper and
`defer release()`s before the helper returns:

| Job | Acquire site | Hold |
|---|---|---|
| Scheduler | `internal/store/postgres.go:1453-1456` via `AcquireSchedulerLock` | **whole goroutine lifetime** |
| Approval reaper | `internal/controller/approval_reaper.go:41` | one tick |
| Stuck-run reaper | `internal/controller/stuckrun_reaper.go:42` | one tick |
| Queued-run reaper | `internal/controller/queuedrun_reaper.go:49` | one tick |
| AppSource sync reaper | `internal/controller/appsource_sync_reaper.go:44` | one tick |
| AppSource reconciler | `internal/controller/appsource_reconciler.go:75` | one tick |
| Log archiver | `internal/controller/archiver.go:40` | one tick |
| Cache cleanup | `internal/controller/scheduler.go:236` | one tick |
| Audit retention | `internal/controller/audit_retention.go:41` | one tick |
| Run retention | `internal/controller/run_retention.go:50` | one tick |
| Log trim | `internal/controller/log_trim.go:48` | one tick |

On an idle stack each of those ticks is one `SELECT` against an empty result
set, so the hold is sub-millisecond against a 30s-to-1h tick. **A census
sampled three times will therefore see the scheduler key and nothing else,
and that is the correct result, not a defect.** The census is still worth
taking — it is the only way to observe the scheduler's *persistent* holder and
to prove no key is doubly held in steady state — but the ten per-tick jobs
need a different instrument (Phase 2).

### (2) The `objid` → key mapping is an inference and must be verified, not assumed

`AcquireAdvisoryLock` (`internal/store/postgres.go:1425-1450`) calls the
**single-argument** `pg_try_advisory_lock($1)` with an `int64`. Standard
Postgres semantics put the high 32 bits of that key in `pg_locks.classid`, the
low 32 bits in `objid`, and `objsubid = 1`. All eleven keys are below 2^31
(they are four ASCII bytes each; the largest is `0x73796E63` = 1937337955), so the
expected shape is `classid = 0`, `objid = key`. **That is read from Postgres's
documented lock representation, not from this repository**, so Phase 1 pins it
empirically: the scheduler's key is the one key whose holder can be
independently attributed (the container that logged `"scheduler became leader"`,
`scheduler.go:55` — the only leadership log line in the system), so matching
that container's IP against `pg_stat_activity.client_addr` for the row with
`objid = 1702388580` verifies both the mapping and the attribution method in
one step. Do not report any other key's holder until that check passes.

### (3) The lock-key table (keys read from source at this branch's HEAD)

| Job | Constant | Hex | Decimal (`objid`) | Interval | Wired in `test/ha`? |
|---|---|---|---|---|---|
| Scheduler | `schedulerLockKey` (`postgres.go:1422`) | `0x65786364` | 1702388580 | 200ms | **yes** |
| Log archiver | `logArchiverLockKey` (`archiver.go:15`) | `0x6C6F6761` | 1819240289 | 30s | **no** — `obj == nil` |
| Cache cleanup | `cacheCleanupLockKey` (`scheduler.go:214`) | `0x63616368` | 1667326824 | 24h | **no** — `obj == nil` |
| Approval reaper | `approvalReaperLockKey` (`approval_reaper.go:14`) | `0x61707276` | 1634759286 | 1m | **yes** |
| Stuck-run reaper | `stuckRunReaperLockKey` (`stuckrun_reaper.go:15`) | `0x7374756B` | 1937012075 | 30s | **yes** |
| Queued-run reaper | `queuedRunReaperLockKey` (`queuedrun_reaper.go:16`) | `0x71756575` | 1903519093 | 30s | **yes** |
| AppSource sync reaper | `appSourceSyncReaperLockKey` (`appsource_sync_reaper.go:14`) | `0x73796E63` | 1937337955 | 30s | **yes** |
| Audit retention | `auditRetentionLockKey` (`audit_retention.go:15`) | `0x61756474` | 1635083380 | 1h | **yes** — default 90 days, but first tick is boot+1h |
| Run retention | `runRetentionLockKey` (`run_retention.go:17`) | `0x7272746E` | 1920103534 | 1h | **no** — flag 0 |
| Log trim | `logTrimLockKey` (`log_trim.go:17`) | `0x6C74726D` | 1819570797 | 1h | **no** — flag 0 |
| AppSource reconciler | `appSourceReconcilerLockKey` (`appsource_reconciler.go:22`) | `0x61707073` | 1634758771 | 30s | **yes** |

**Why four of the eleven are not wired here, and a fifth is unobservable —
determine this from the stack, do not inherit it.**
`test/ha/docker-compose.ha.yaml:19-25` sets only `UNIFIED_DB_DSN`,
`UNIFIED_TOKEN` and `UNIFIED_CONTROLLER_KEY_FILE`, and
`docker/controller.Dockerfile` adds only `UNIFIED_WEB_DIR`. So:

- No `UNIFIED_S3_*` and no `UNIFIED_DATA_DIR` ⇒ `obj == nil`
  (`cmd/controller/main.go:303-322`) ⇒ the two `obj != nil`-gated goroutines
  at `main.go:399-402` are **never started**. Boot-log fingerprint:
  `"no object store configured — log archival disabled"` (`main.go:321`).
- No `UNIFIED_RUN_RETENTION_DAYS` / `UNIFIED_LOG_TRIM_DAYS` ⇒ both flags
  default to 0 (`main.go:47-58`, `:64-75`) ⇒ `RunRunRetention`
  (`run_retention.go:29`) and `RunLogTrim` (`log_trim.go:29`) **return before
  creating their tickers**, so they never even attempt the lock. Boot-log
  fingerprint: `"run retention disabled (keep forever)"` (`main.go:424`) and
  `"log trim disabled (DB log rows kept forever)"` (`main.go:434`).
- **Audit retention is the exception, and the W2 plan's hint that it "needs its
  flag" is wrong:** `auditRetentionDaysDefault()` (`main.go:26-41`) falls back
  to **90 days** when `UNIFIED_AUDIT_RETENTION_DAYS` is unset, so the job *is*
  started here. Boot-log fingerprint: `"audit log retention enabled"
  retentionDays=90` (`main.go:416`). Its ticker is 1h, so its **first
  acquisition is at boot + 1h** — outside any practical observation window.
  Record it as *wired but unobserved*, which is a third category distinct from
  both "held by exactly one replica" and "not wired".

**A key with zero holders because its goroutine was never started — or because
its first tick has not arrived — is a null result, not a violation.** Confirm
each fingerprint in the Baseline gate before treating any absence as
meaningful.

### (4) The three jobs with no leader election — and what the docs say about them

- `RunGitResolver` — `cmd/controller/main.go:437-445`, 200ms tick;
  `resolveGitPendingRuns` (`internal/controller/scheduler.go:290`) acquires no
  lock of any kind.
- `DeleteStaleAgents` — the inline loop at `cmd/controller/main.go:382-398`,
  1m tick, 5m threshold; `postgres.go:1225-1233` is a bare
  `DELETE FROM agents WHERE last_seen_at < NOW() - $1`.
- `DeleteExpiredOIDCStates` — the inline loop at `main.go:368-381`, 10m tick;
  `postgres.go:1912-1915` is a bare `DELETE FROM oidc_states WHERE expires_at <= NOW()`.

**This is where the W2 plan's second premise fails.** The plan lists these as
"three background jobs [that] have NO leader election" and its Recording rule
calls their unlocked state a **violation**. But `docs/high-availability.md:79-92`
is an explicit arbitration table, and **two of the three are in it as
deliberate design**:

```
| Git resolver (`RunGitResolver`) | none (idempotent) | Runs on all replicas. `git://` URI resolution is idempotent and harmless if duplicated |
| OIDC state cleanup | none (idempotent) | Runs on all replicas. Expired state deletion is idempotent |
```

So for those two the absence of a lock **is** the documented contract, and
finding them unlocked confirms the docs rather than contradicting them. The
open question is narrower and must be asked precisely: **is the documented
claim "harmless if duplicated" true?** Answer it from the code with `file:line`
and say plainly what was and was not demonstrated live.

`DeleteStaleAgents` is the one of the three that is **not** in that table, and
its arbitration is not stated anywhere in `docs/` — `docs/high-availability.md:374`
and `:443` describe its threshold and its interaction with the stuck-run reaper
but never say whether it is leader-elected. The stuck-run reaper is the mirror
image: also absent from the table, but its leader election is described in
prose at `docs/high-availability.md:380-395`. So the table is not exhaustive in
either direction, and a reader cannot infer an omitted job's arbitration from
its absence.

## Baseline (plain stack, before any observation)

```bash
cd test/ha
docker compose -f docker-compose.ha.yaml up -d --build
```

BASELINE GATE — confirm all of these before recording anything. If any fails,
STOP and report BLOCKED with the evidence.

```bash
SCRATCH=<scratchpad>/w2-1 ; mkdir -p "$SCRATCH"

# 1. LB and all three controllers up
curl -s -o /dev/null -w 'readyz=%{http_code}\n' localhost:18080/readyz          # expect 200
docker compose -f docker-compose.ha.yaml ps --format '{{.Service}} {{.State}}'  # 3 controllers Up

# 2. Exactly one leader, and know which container it is
for c in controller1 controller2 controller3; do
  echo "== $c"
  docker compose -f docker-compose.ha.yaml logs $c | grep -c "scheduler became leader"
done                                          # expect exactly one controller with >= 1

# 3. The five not-wired fingerprints (see mechanism note 3)
docker compose -f docker-compose.ha.yaml logs controller1 \
  | grep -E "no object store configured|retention|log trim"
# expect 4 lines: no-object-store (WARN), "audit log retention ENABLED
# retentionDays=90", "run retention disabled", "log trim disabled".  Their
# presence is what makes a missing advisory-lock holder a null result rather
# than a finding.  Note the audit line says *enabled* — see mechanism note 3.

# 4. Both agents registered (needed only for Phase 3)
curl -fsS localhost:18080/api/v1/agents -H "Authorization: Bearer ha-admin-token"
```

Record the container→IP map now; every later attribution depends on it:

```bash
for c in controller1 controller2 controller3; do
  cid=$(docker compose -f docker-compose.ha.yaml ps -q $c)
  echo "$c $(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' $cid)"
done | tee "$SCRATCH/controller-ips.txt"
```

## Phase 0 — enable statement logging

```bash
docker compose -f docker-compose.ha.yaml exec -T postgres psql -U unified -c \
  "ALTER SYSTEM SET log_statement='all'; ALTER SYSTEM SET log_line_prefix='%m [%p] h=%h '; SELECT pg_reload_conf();"
docker compose -f docker-compose.ha.yaml exec -T postgres psql -U unified -c "SHOW log_statement;"
# expect: all
```

Every `pg_try_advisory_lock` and `pg_advisory_unlock` is now recorded with a
timestamp, the backend pid, and the client host — i.e. **which replica tried,
which replica won, and exactly how long it held the lock**. This is a complete
record, not a sample, which is what makes the negative provable.

pgx uses the extended protocol, so each call appears as two lines:

```
2026-07-30 ... [123] h=172.x.y.z LOG:  execute <name>: SELECT pg_try_advisory_lock($1)
2026-07-30 ... [123] h=172.x.y.z DETAIL:  parameters: $1 = '1702388580'
```

**Verify that the `DETAIL: parameters` line is actually emitted before relying
on it** (`log_parameter_max_length` defaults to -1 = log in full, but confirm
rather than assume). If parameters are absent, fall back to the high-frequency
`pg_locks` sampler in Phase 2b.

## Phase 1 — `pg_locks` census (three samples, ~30s apart)

```bash
census() {
docker compose -f docker-compose.ha.yaml exec -T postgres psql -U unified -x -c "
WITH keys(job, k) AS (VALUES
  ('01 scheduler',            x'65786364'::bit(32)::bigint),
  ('02 log-archiver',         x'6C6F6761'::bit(32)::bigint),
  ('03 cache-cleanup',        x'63616368'::bit(32)::bigint),
  ('04 approval-reaper',      x'61707276'::bit(32)::bigint),
  ('05 stuckrun-reaper',      x'7374756B'::bit(32)::bigint),
  ('06 queuedrun-reaper',     x'71756575'::bit(32)::bigint),
  ('07 appsource-sync-reaper',x'73796E63'::bit(32)::bigint),
  ('08 audit-retention',      x'61756474'::bit(32)::bigint),
  ('09 run-retention',        x'7272746E'::bit(32)::bigint),
  ('10 log-trim',             x'6C74726D'::bit(32)::bigint),
  ('11 appsource-reconciler', x'61707073'::bit(32)::bigint))
SELECT k.job, k.k AS key, l.pid, l.granted, l.classid, l.objsubid,
       a.client_addr, a.backend_start
FROM keys k
LEFT JOIN pg_locks l ON l.locktype='advisory' AND l.objid::bigint = k.k
LEFT JOIN pg_stat_activity a ON a.pid = l.pid
ORDER BY k.job;"
}
for i in 1 2 3; do date -u +%FT%T.%3NZ; census; sleep 30; done \
  | tee "$SCRATCH/census.txt"
```

Also dump every advisory lock present, so an *unexpected* key would be visible:

```bash
docker compose -f docker-compose.ha.yaml exec -T postgres psql -U unified -c \
 "SELECT locktype, classid, objid, objsubid, pid, granted FROM pg_locks WHERE locktype='advisory' ORDER BY objid;" \
 | tee -a "$SCRATCH/census.txt"
```

**The mapping check (do this before interpreting anything else):** take the
`client_addr` on the `01 scheduler` row and look it up in
`controller-ips.txt`. It must be the same container that logged
`"scheduler became leader"`. If it is not, the `objid`→key inference is wrong
and everything downstream must be re-derived — that is itself the finding.

Expected shape, per mechanism note 1: **one row with a pid (scheduler), ten
rows with `pid` NULL**, at all three samples. Of those ten, **five** are wired
and actively ticking (approval, stuck-run, queued-run and AppSource-sync
reapers, plus the AppSource reconciler) but hold their locks for ~1ms;
**one** (audit retention) is wired but has not reached its first hourly tick;
**four** (log archiver, cache cleanup, run retention, log trim) are not wired.

## Phase 2 — statement-log audit of the per-tick jobs

Let the stack idle for **at least 5 minutes** after Phase 0 so that every
30s job ticks ~10 times and the 1m approval reaper ticks ~5 times. Then:

```bash
docker compose -f docker-compose.ha.yaml logs --no-log-prefix postgres > "$SCRATCH/postgres-full.log"
grep -n "advisory_lock\|advisory_unlock\|parameters: \$1" "$SCRATCH/postgres-full.log" > "$SCRATCH/advisory-raw.txt"
wc -l "$SCRATCH/advisory-raw.txt"
```

Reduce it to `(timestamp, pid, host, verb, key)` tuples and answer three
questions. The reduction script belongs in the scratchpad, not in the repo;
what matters is that the three answers each cite a file.

1. **Did every replica contend?** Count `pg_try_advisory_lock` calls per
   `(host, key)`. For each wired per-tick job, all three controller IPs must
   appear with roughly equal counts (≈ one per tick each). If only one IP ever
   calls, the other two are not running the job at all and the "leader
   election" is vacuous — a different defect from the one being tested.
2. **Did more than one replica ever hold the same key at once?** For each key,
   pair each `pg_try_advisory_lock` with the next `pg_advisory_unlock` on the
   **same pid** and treat that as a hold interval; a `try` with no matching
   unlock on that pid is a losing attempt. Then check for overlapping intervals
   across pids on the same key. **Any overlap is a major I1 violation.**
3. **Was every tick covered?** Per key, count the distinct tick instants (group
   the `try` calls into clusters) and confirm exactly one winner per cluster.
   Zero winners in a cluster would mean a tick where nobody did the work.

Also confirm the scheduler's key never appears as a *successful* second
acquisition: the leader holds it continuously, so the two non-leaders should
show a `pg_try_advisory_lock` every 200ms with no unlock ever.

### Phase 2b — fallback if parameters are not logged

If the `DETAIL: parameters` line is absent, sample `pg_locks` in-database at
high frequency instead (this catches millisecond holds where a shell loop
cannot):

```bash
docker compose -f docker-compose.ha.yaml exec -T postgres psql -U unified -c "
CREATE SCHEMA IF NOT EXISTS edgecase;
CREATE TABLE IF NOT EXISTS edgecase.locksample(t timestamptz, objid bigint, pid int);
DO \$\$ BEGIN FOR i IN 1..60000 LOOP
  INSERT INTO edgecase.locksample SELECT clock_timestamp(), objid::bigint, pid
    FROM pg_locks WHERE locktype='advisory';
  PERFORM pg_sleep(0.005);
END LOOP; END \$\$;"
```

then group by `(objid, t)` and look for any instant with two distinct pids.
Note in the findings that this is a *sample*, so absence of an overlap is
weaker evidence than the statement log's completeness.

## Phase 3 — the three unlocked jobs

**3a. Confirm the absence of a lock, from both sides.** From `pg_locks` (Phase
1: no key exists for them at all — they never call `AcquireAdvisoryLock`), and
from the statement log: the three jobs' own statements must appear from **all
three** controller IPs.

```bash
grep -c "DELETE FROM agents WHERE last_seen_at" "$SCRATCH/postgres-full.log"
grep -c "DELETE FROM oidc_states" "$SCRATCH/postgres-full.log"
```

Split those counts by host. This is direct evidence of concurrent execution —
strictly better than the plan's proposed inference from the `deleted=N` log
line, which (as the plan itself notes) cannot distinguish "one replica ran"
from "three raced and two saw N=0".

**3b. Drive `DeleteStaleAgents` to actually delete something.** Kill one agent
so its `last_seen_at` ages past the 5-minute threshold.

**Use `kill -s SIGKILL`, not `stop`.** `docker compose stop` sends SIGTERM, and
a healthy agent's shutdown path **deregisters itself** — it issues
`DELETE FROM agents WHERE id = $1` and logs `"agent deregistered"`, so the row
vanishes within ~1s and `DeleteStaleAgents` is never exercised at all. That is
a real property worth recording once, but it is the wrong instrument for this
probe.

```bash
date -u +%FT%T.%3NZ
docker compose -f docker-compose.ha.yaml kill -s SIGKILL agent2
# wait > 5 min from the agent's LAST heartbeat (not from the kill); sample every 30s
for i in $(seq 1 14); do
  date -u +%FT%T.%3NZ
  docker compose -f docker-compose.ha.yaml exec -T postgres psql -U unified -tAc \
    "SELECT id, last_seen_at, NOW()-last_seen_at FROM agents ORDER BY id;"
  sleep 30
done | tee "$SCRATCH/stale-agent-timeline.txt"

docker compose -f docker-compose.ha.yaml logs controller1 controller2 controller3 \
  | grep "deleteStaleAgents" | tee "$SCRATCH/deletestaleagents.txt"
```

Record all of:

- the exact minute in which the row disappeared, from the timeline;
- how many **controllers logged `deleteStaleAgents deleted=N`** (`main.go:393-395`,
  emitted only when `n > 0`) — expect exactly **one**, because the DELETE is
  idempotent;
- how many **`DELETE FROM agents` statements ran in that same minute**, from
  the statement log, split by host — expect **three**, one per replica.

**State the limitation explicitly:** the single `deleted=1` line on its own
cannot distinguish "one replica ran" from "three raced and two saw N=0". It is
the statement log that discriminates them, and that is why it was captured.

**3c. `RunGitResolver` — code-read only.** Demonstrating harm needs a
git-backed job fixture; this campaign has none (W1-7 was deferred for exactly
this reason). **Do not fabricate one.** Record with `file:line`:

- no lock at `scheduler.go:290` (`resolveGitPendingRuns`), tick 200ms on every
  replica (`main.go:437-445`);
- the `failureBackoff` at `scheduler.go:271-274` is created **per goroutine**,
  so its documented purpose — "a poisoned candidate ... doesn't fill every
  batch" — holds only within one replica; a candidate excluded on replica A is
  still selected by B and C. Note that this comment is on an **unexported
  helper**, so per the campaign's classification rule it is *not* a documented
  contract and cannot on its own carry a violation;
- the deadline path at `scheduler.go:338-348` does `AppendLog` **then**
  `MarkRunFinished` with no lock and no CAS, so N replicas can each append the
  same system log line;
- `ListPendingRuns(ctx, 50, ...)` at `scheduler.go:291` is the same 50-row
  snapshot W2-9 is about.

Then state plainly: **concurrent-execution harm was not demonstrated live**,
and carry it forward as a scenario needing the git-over-HTTP fixture.

## Phase 4 — leader failover

```bash
# leader identified in the Baseline gate and confirmed by Phase 1's client_addr
date -u +%FT%T.%3NZ
docker compose -f docker-compose.ha.yaml kill -s SIGKILL controller<N>   # hard kill
for i in $(seq 1 12); do
  date -u +%FT%T.%3NZ
  docker compose -f docker-compose.ha.yaml logs --since 5m controller1 controller2 controller3 \
    | grep "scheduler became leader"
  sleep 5
done | tee "$SCRATCH/failover.txt"
```

Then re-run the Phase 1 census and record:

- **who** acquired the scheduler lock (must be a different container), and
  **when** relative to the kill — the pid on the `01 scheduler` row must change
  and its `client_addr` must be the new leader;
- that the dead controller's backend is gone from `pg_stat_activity` (Postgres
  releases session locks when the connection dies);
- that no key is left with two holders and no wired key is left permanently
  unheld — re-run Phase 2's overlap check over the post-failover window.

## Recording (severity guidance)

- **Two replicas holding the same advisory key simultaneously = major (I1).**
  This is the pass/fail. Evidence must be the statement log's interval overlap
  or two pids on one `objid` in a single `pg_locks` row set — not an inference.
- **A wired job with no holder across the whole observation window = major**
  (the job is not running at all). Applies only to the six wired-and-ticking
  keys; the four un-wired ones and audit retention (wired, first tick at
  boot+1h) are null results and must be reported as such, with the
  boot-log fingerprint that proves they were never started.
- **The three unlocked jobs.** Do **not** file a blanket violation. Judge each:
  - Git resolver and OIDC cleanup are **documented** as unlocked-and-idempotent
    (`docs/high-availability.md:91-92`). Confirming they are unlocked is a null
    result. A finding here requires showing the "harmless if duplicated" claim
    is false, and any such argument that is code-read only must say so.
  - `DeleteStaleAgents` is undocumented in either direction — that gap is at
    most **minor** (diagnosability), and the DELETE's idempotence is the reason
    it is not worse.
- **The census expectation in the plan (one holder per key per sample) is
  wrong for ten of the eleven keys** and the runbook says why. Record the
  correction; do not manufacture a product defect out of it.
- Failover leaving a key orphaned or doubly held = **major (I1)**. Failover
  latency alone, if bounded, = observation with the measured number.

## Execution notes (added after the 2026-07-29 run — read before re-running)

- **The instrumentation works exactly as designed and is the whole scenario.**
  `log_parameter_max_length` is `-1` on `postgres:16-alpine`, so the
  `DETAIL: parameters: $1 = '<key>'` line is emitted for every prepared-statement
  execution and Phase 2b was never needed. pgx uses named prepared statements,
  so the pair reads `execute stmtcache_<hash>: SELECT pg_try_advisory_lock($1)`
  followed by the `DETAIL`. Cost on an idle three-replica stack: **~40MB of
  Postgres log per 26 minutes** (~667k lines), compressing to ~0.9MB. Capture
  with `docker compose logs --no-log-prefix postgres` and gzip before copying
  to the evidence root.
- **Budget ~30 minutes of wall clock**, dominated by two 5-minute-plus waits
  (the idle observation window and the `DeleteStaleAgents` threshold). The
  build was cached and `up -d --build` took 64s.
- **The plan's census expectation was confirmed wrong, exactly as mechanism
  note 1 predicted.** All eight censuses returned a single advisory-lock row
  (the scheduler's). Do not read that as ten dead jobs — the statement log
  showed 15,665 lock events over the same period.
- **`docker compose stop agent2` does not make an agent go stale.** The agent
  deregisters itself on SIGTERM (`internal/agent/agent.go:349`); its row was
  gone 0.9s later and `DeleteStaleAgents` was never exercised. Use
  `kill -s SIGKILL` and start the 5-minute clock from the agent's **last
  heartbeat** (`agents.last_seen_at`), not from the kill.
- **Audit retention is wired on this stack** — `auditRetentionDaysDefault()`
  returns 90 when the env var is unset — but its first tick is boot+1h, so it
  contributes nothing to a half-hour observation. Its exclusion behaviour is
  the one part of the eleven this scenario did not exercise.
- **Failover is fast enough that a 3s poll cannot resolve it.** The new leader
  was elected 0.40s after the kill; the measurement that matters came from the
  statement log (failed poll at `+0.196s`, successful poll at `+0.396s`), not
  from the polling loop. Read the failover latency from the log, not the loop.
- **Teardown caveat:** `SHOW log_statement` issued in the same `psql`
  invocation as `ALTER SYSTEM RESET ...; SELECT pg_reload_conf()` still
  reported `all` — that backend had not processed the SIGHUP yet. It is not
  evidence the reset failed, and it is moot here because `down -v` destroys
  the data directory (and `postgresql.auto.conf`) immediately afterwards.

## Teardown

```bash
docker compose -f docker-compose.ha.yaml exec -T postgres psql -U unified -c \
  "ALTER SYSTEM RESET log_statement; ALTER SYSTEM RESET log_line_prefix; SELECT pg_reload_conf();"
docker compose -f docker-compose.ha.yaml down -v
docker compose -f docker-compose.ha.yaml ps -a
```

The `ALTER SYSTEM RESET` is belt-and-braces — `down -v` destroys the Postgres
data directory (and therefore `postgresql.auto.conf`) anyway — but running it
explicitly keeps the scenario safe to re-run without `-v`, and documents that
the instrumentation was reverted.
