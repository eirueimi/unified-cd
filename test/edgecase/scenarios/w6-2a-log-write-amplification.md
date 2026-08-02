# W6-2a — the per-line log write-path amplification

**Wave W6, Task 2.** The cheapest scenario in the wave: one `edge-logburst`
run and a Postgres statement-log capture, repeated with 0, 1, 5 and 10 SSE
subscribers attached. It measures what one 2,000-line log burst *costs the
database*, and how that cost scales with the number of people watching the
run in a browser.

The defect this scenario measures is **not** "the bulk append is a per-line
loop" — `FINDINGS.md:1521` (W3-4) already filed that, as an I4 violation about
**duplication under retry**. This scenario measures the same loop's **cost**:
how many round trips and how many `NOTIFY`s one request buys, what a
subscriber multiplies that by, and what — if anything — bounds the request
that starts it. **Cite W3-4; this does not re-file it.**

**Invariants attacked: expected to be none.** I4 is about a `Succeeded` run's
line count, duplicates and reordering; nothing here injects a fault, so the
count is expected to be exact and the entry is expected to rest on a
**documented-contract** limb or on nothing at all. `FINDINGS.md:1509` is the
governing rule: *"before citing an invariant or a doc passage, quote it
verbatim, read the surrounding section, and check the direction — an invariant
must be contradicted by its own text (not by its spirit), and a doc sentence
must forbid what you observed rather than describe or permit it."*

---

## Corrections to inherited facts, established BEFORE execution

Per the W1-W5 carry-forward rule, the brief's mechanism block is a set of
**claims**. Every one was re-read at this branch's HEAD before the rig was
started. **The pattern held for a seventh consecutive wave: every `file:line`
claim is correct, and the arithmetic drawn from them needed two amendments.**

### The `file:line` claims — all four hold, verbatim

| Claim | Verified at HEAD |
|---|---|
| `internal/controller/api_agent.go:721-734` — `handleAgentLogBulk` loops over every line calling `s.store.AppendLog` individually | **HOLDS.** `for _, req := range lines {` is line 721; its closing `}` is line 734; `AppendLog` is called at line 725. No transaction is opened anywhere in the handler |
| `internal/store/postgres.go:918-936` — each `AppendLog` is one `QueryRow` plus one `pg_notify` | **HOLDS.** `func (p *Postgres) AppendLog` is line 918, `p.pool.QueryRow(...)` line 926, `p.pool.Exec(ctx, "SELECT pg_notify($1, $2)", ...)` line 935, closing brace 937 |
| `internal/controller/sse.go:118-143` — every NOTIFY wakes every SSE subscriber, each wake issuing `TailLogs(..., 10_000)` plus a `GetRun` | **HOLDS.** `ListenForNotify` at 118, `TailLogs(dbCtx, id, lastSeq, 10_000)` at 120, `GetRun(dbCtx, id)` at 138, callback closes at 143 |
| `internal/controller/api_webhooks.go:118` is the **only** `MaxBytesReader` in the tree | **HOLDS, and the enumeration was re-run as instructed.** `grep -rn "MaxBytesReader" --include="*.go" .` returns **exactly 1 hit**, and it is that line. Hit count reported, not truncated (`w6-2a/codesurvey.txt`) |

### AMENDMENT 1 — the per-request cost is `2N + G`, not `2N`; and `G` is bounded by the number of replicas, not by the number of requests

The brief's `2N` counts the two statements inside `AppendLog`. It omits the
**guard loop** at `api_agent.go:705-718`, which runs `agentRunGuard` **once per
distinct `runID` in the batch** — `guarded[req.RunID]` short-circuits the rest.
`agentRunGuard` with `rejectTerminal=false` (`agent_guard.go:98-105`) answers
from an in-process LRU (`s.claimedBy`) when the run's owner is already cached,
and otherwise issues one `GetRun`.

**Consequence, and it is small but it changes the shape of the prediction:**
for a single-run batch the guard costs **one `GetRun` the first time that
controller sees the run, and zero thereafter**. With nginx round-robining the
agent's bulk requests across three replicas, `G` is expected to be **≤ 3 per
run for the whole run**, not per request and not per line. It is a constant,
not a term that scales with `N`. The prediction is therefore
**`2N + G`, `G ≤ 3`** — which for `N = 2002` is a 0.07% correction and is
recorded for correctness, not because it moves the number.

### AMENDMENT 2 — the SSE wake does not spend the *listen* pool, it spends the **API** pool, and that is the load-bearing fact of this scenario

The brief's `2N + 2NS` is arithmetically right and **silent about where the
`2NS` lands**, which is the part with a documented contract attached to it.

`ListenForNotify` (`postgres.go:1665-1677`) takes a connection from the
**listen** pool and holds it for the stream's life. But the callback it is
handed does **not** use that connection. `handleRunEvents` calls
`s.store.TailLogs` and `s.store.GetRun` (`sse.go:120`, `:138`), and `s.store`
is the server's ordinary store: `cmd/controller/main.go:270` builds
`st := metrics.NewInstrumentedStore(pg, m)` from the **api-pool** `*Postgres`
and passes exactly that to `controller.NewServer` (`main.go:339`). The
background view (`pg.BackgroundStore()`, `main.go:271`) is a *different*
object and is never given to the HTTP server.

**So every one of the `2NS` queries is an API-pool acquisition made on behalf
of an SSE subscriber**, on the replica that subscriber connected to. This is
predicted here and measured in Part B.

### AMENDMENT 3 — `MaxBytesReader` is not the only thing that could bound the direct path, and the others are absent too

The brief asks only about `MaxBytesReader`. Enumerated at HEAD, the three other
places a Go HTTP server can bound a request body are also empty on this path:

- **No `ReadTimeout` on the controller's `http.Server`.** `cmd/controller/main.go:451-455`
  sets `ReadHeaderTimeout: 10 * time.Second` and nothing else — no `ReadTimeout`,
  no `WriteTimeout`, no `MaxHeaderBytes` override.
- **No body-limiting middleware.** The chain is `middleware.Recoverer`,
  `middleware.RealIP`, `accessLogMiddleware`, `s.metricsMiddleware`,
  `securityHeadersMiddleware`, `s.originCheckMiddleware`
  (`internal/controller/server.go:285-293`), plus `ServerAuth` and
  `auditLogMiddleware` on the authenticated groups. None reads or wraps
  `r.Body`.
- **No `io.LimitReader` on the handler.** `handleAgentLogBulk` calls
  `json.NewDecoder(r.Body).Decode(&lines)` (`api_agent.go:699`) directly.

Part C measures this rather than asserting it.

---

## The rig

`test/ha` plus two overlays:

```bash
cd test/ha
export COMPOSE_FILES="-f docker-compose.ha.yaml \
  -f ../edgecase/compose/logfault.override.yaml \
  -f ../edgecase/compose/ctrlports.override.yaml"
docker compose $COMPOSE_FILES up -d --build
```

- `ctrlports.override.yaml` is **required**: SSE is taken straight against
  `controller3` on `:18083`, never through the LB. `README.md:59-62` records
  that an nginx reload severs in-flight SSE streams, and a capture at the mercy
  of an unrelated reload is not a capture.
- `logfault.override.yaml` is used for its **access-log format only**, not for
  any fault: the `logfault` format leads with `$msec` and stamps `arm=` and
  `reqlen` onto every request, which is what lets the agent's bulk requests be
  counted and sized individually. **No fault is armed at any point in this
  scenario**; `w3-4-logfault.sh` is not invoked.

Harnesses, all from Task 1 and used as-is:

- `tools/w6/w6-idleload.sh` + `w6-idleanalyze.py` — the statement-log capture.
  Despite the name it is a **generic** window recorder: it arms
  `log_statement='all'` and `log_line_prefix='%m [%p] host=%h '` with one
  `ALTER SYSTEM` per `psql -c`, verifies both in a **fresh** session, captures
  for `-d` seconds, **always reverts on a trap**, and hands the raw log to a
  separable analyser. Its "leave the stack alone" instruction is what makes the
  idle arm an idle arm; the four loaded arms deliberately do the opposite and
  say so.
- `tools/w6/bin/ssehold` — S SSE streams against one named controller, with
  per-stream alive-at-end. Built by `w6-build.sh` (never `go run`).
- `tools/w6/w6-pgsample.sh` — `pg_stat_activity` on a grid, per replica and per
  **derived** pool. Run only during the S=10 arm, because it is itself psql
  traffic; its statements are attributable and are subtracted (see Part B).

Fixture: `workloads/logburst.payload.json`, job `edge-logburst` — `burst-begin`,
`sleep 8`, `burst-1`…`burst-2000` as fast as the shell can, `sleep 30`,
`burst-end`. **N = 2002 lines.**

---

## Method

Five capture windows, each one `w6-idleload.sh -d <D> -l <label>` run in the
background while the foreground drives the arm:

| Arm | S | What runs inside the window |
|---|---|---|
| `floor` | — | nothing. The idle floor, re-confirmed rather than inherited |
| `A-s0` | 0 | one `edge-logburst` run, no subscribers |
| `B-s1` | 1 | one run, 1 SSE stream on `:18083` |
| `B-s5` | 5 | one run, 5 SSE streams on `:18083` |
| `B-s10` | 10 | one run, 10 SSE streams on `:18083`, plus `w6-pgsample.sh` |

Subscribers are attached **immediately after the trigger returns**, not after
the run starts: `handleRunEvents` proceeds to `LISTEN` for any non-terminal
run, and the fixture's `sleep 8` guarantees the burst has not begun. Every arm
therefore has all S streams established before the first `burst-N` line exists.

Every number is reported **net of the `floor` arm**, and each arm's own
statement classes are used to separate the log path from everything else, so
the subtraction is checkable rather than asserted.

### Predictions, stated before the first capture

With `N = 2002` and the amendments above:

| Arm | Predicted log-path statements | Of which `pg_notify` | Of which SSE-side |
|---|---:|---:|---:|
| `A-s0` | `2N + G` = **4004 + (≤3)** | 2002 | 0 |
| `B-s1` | `2N + 2NS + G` = **8008 + (≤3)** | 2002 | 4004 |
| `B-s5` | **24024 + (≤3)** | 2002 | 20020 |
| `B-s10` | **44044 + (≤3)** | 2002 | 40040 |

Also predicted, and to be checked rather than assumed:

1. The `2NS` lands on the **API** pool of `controller3` (Amendment 2).
2. Nothing bounds the request body on the direct-to-controller path (Part C).
3. Exactly one product code path `LISTEN`s on `log_appended:*` (Part D), and
   the live capture is what establishes that, not the grep.

### The connection-budget hazard, inherited and watched

Task 1 measured **73-74 of Postgres's 100 backends in use at rest** and found
that **~2,554 req/s held at 8 in flight pins the server at `max_connections`**
— the trigger is the **rate**, not the concurrency bound, and
`FINDINGS.md:2535` says explicitly "do not read this entry as '8 concurrent
requests at any rate will do this'" — with
`/readyz` still 200 and a valid admin PAT returning 401 — filed as the
`W6-infra` observation at `FINDINGS.md:2517`. **That entry is cited, not
re-filed.** The S=10 arm adds 10 `listen` connections plus up to 10 concurrent
API-pool acquisitions on one replica against ~26 free slots, so the arm is
instrumented with `w6-pgsample.sh` and any anomalous number is checked against
the connection count before it is believed.

---

## Parts

- **Part A — the base cost.** Measure the actual statement count and
  `pg_notify` count for one run against the predicted `2N + G`, net of the
  floor. Report the arithmetic.
- **Part B — the subscriber multiplier.** S = 0, 1, 5, 10 against `2N + 2NS + G`,
  with the pool attribution of Amendment 2 measured.
- **Part C — the unbounded body.** Establish what actually bounds a log-bulk
  request on the **direct-to-controller** path. nginx's 1 MiB default is
  already filed at `FINDINGS.md:1655` — **cited, not re-filed**.
- **Part D — the NOTIFY consumers.** Enumerate them from the capture, not from
  the grep, and state what each wake costs.

---

## RESULTS

Executed 2026-08-02 `04:46:01Z – 05:33:21Z` on branch `plan/edge-case-w6`.
Seven Postgres statement-log windows, ~2.1 million raw log lines. Every number
below traces to a capture whose window covers it; the raw captures are gzipped
under `w6-2a/<arm>/` in the evidence root and the per-arm derivations are in
`w6-2a/<arm>/breakdown.txt`.

### An instrument defect found and fixed before any number was trusted

**`w6-idleload.sh` did not stop its own capture.** It backgrounded
`dc logs -f ... > "${raw}" &` and killed `$!`, but `dc` is a shell function, so
`$!` is the subshell — the process holding the pipe is the `docker-compose`
CLI **plugin** two levels down, and it survives. Measured
(`w6-2a/leaked-followers.txt`): after three arms, **three** plugin processes
from three "finished" captures were still writing. The `floor` file read
**10,948** statements when its own analyser ran and **26,523** when re-read
after the next arm — it had silently absorbed a later window, and the second
number looks exactly as plausible as the first.

Two things follow, both done:

1. **Every derivation in this scenario is window-bounded.** `breakdown.py` takes
   the report's own `window` line as `[T0, T1]` and refuses to count outside it.
   With the filter the `floor` file re-reads at **10,948** — identical to what
   its own analyser saw live.
2. **`w6-idleload.sh` is fixed** to take the window as a bounded pull
   (`docker compose logs --since T --until T`) with no background process at
   all, and to write the window to a `-window.txt` sidecar so an old file can
   bound its own re-analysis. **Verified live**, because "the code is present"
   is never the evidence (`w6-2a/harnessfix/verify.txt`): two consecutive 25 s
   windows against a 1 Hz probe captured probes **5–20** and **27–42** — disjoint,
   non-empty, and window 1's file was byte-identical before and after window 2
   ran. The first verification attempt captured **0 lines** and is kept in the
   same file: with only `postgres` up there was no traffic to capture, so the
   run proved nothing and the probe was added.

**A second instrument caveat, smaller.** `w6-pgsample.sh`'s summary line reads
`TOTAL backends: min=100 max=100 ... of max_connections=100`, which reads as
saturation and is not. `pg_stat_activity` includes Postgres's own background
workers, which have a NULL `datname` and **do not consume `max_connections`
slots**. Measured directly at `w6-2a/post-bs10-connstate.txt`: 95 rows for
`datname='unified'`, 1 more `unified` user row, and **4 NULL-`datname` rows** —
so the client-backend count was **96 of 100** with
`superuser_reserved_connections = 3`, i.e. one free non-superuser slot, not
zero. Any "at max_connections" claim from that line must be checked against the
`datname` split.

### The idle floor, re-confirmed rather than inherited

150 s, zero runs, `logfault` + `ctrlports` overlays (neither changes controller
configuration — one replaces nginx's log format, the other publishes ports that
already existed inside the network).

**Both columns divide by their TRUE window, not their nominal one.** The nominal
150 s window actually spans `04:46:10.038 .. 04:48:42.077` = **152.04 s**
(`w6-2a/floor/breakdown.txt`; `w6-2a/floor/persec.txt` independently reports
`seconds observed=152`), and Task 1's nominal 300 s spans **301.5 s**. Dividing
by the nominal figure inflates every rate by ~1 %, and both columns below were
originally reported that way — 72.99 and 72.11. Corrected, they agree more
closely, not less.

| Quantity | Task 1 (300 s nominal / **301.5 s** true) | This scenario (150 s nominal / **152.04 s** true) |
|---|---|---|
| Total | 72.11 q/s nominal → **71.76 q/s** true | 72.99 q/s nominal → **72.01 q/s** true (10,948 / 152.04 s) |
| Per replica | 24.10 / 23.68 / 24.32 (nominal) | **24.15 / 23.99 / 23.86** true (3,671 / 3,647 / 3,627 statements) |
| Postgres backends | 73–74 at rest | 69 at window open, 74 at close |
| Log-path statements | — | **0**, all classes (the floor is a floor) |

**0.4 % apart on the true windows, one day and one wave later** (the
divide-by-nominal figures were 1.2 % apart, so the correction *strengthens* the
agreement). Per-second on `controller3` the
floor is **median 22, max 61** statements/s (`w6-2a/floor/persec.txt`) — the
comparison Part B's peak is measured against.

---

## Part A — the base cost. `2N` measured exactly; `G` is 2, not 3

One `edge-logburst` run (`98460056`, `Succeeded`, 38 s), no subscribers, 150 s
window (`w6-2a/A-s0/`).

```
INSERT INTO logs(run_id, step_index, stream, ts, line) ...   2002
SELECT pg_notify($1, $2)                                     2002
                                                           -----
                                                            4004   = 2N, exactly
```

**N = 2002 and the arithmetic closes on both sides.** `SELECT count(*), min(seq),
max(seq) FROM logs WHERE run_id=...` gives `2002 | 1 | 2002`, and the duplicate
check (`GROUP BY line, ts HAVING count(*) > 1`) gives **0** — so `2N` is not an
inflated count of a duplicated stream. **I4 held**: count matches, no
duplicates, no reordering, run `Succeeded`.

Net of the floor: the window carried **15,251** statements against a floor of
10,948, a delta of **4,303**, of which **4,004 — 93.0 % — is the bare append
path**. The remaining 299 is the run's own lifecycle plus this scenario's own
2 s status polling.

**`G` (Amendment 1) is measured, and it is 2.** In this arm the guard's
`GetRun`s are not separable from the driver's own polling (31 `GetRun`s,
10/10/11 across replicas). **Part C2b settles it cleanly** — no polling there,
and the whole run's guard cost was `getrun controller1 n=1` +
`getrun controller3 n=1` = **2**, one per replica that saw the run for the first
time, against 35,100 lines. The prediction `G ≤ 3` holds and the term is a
constant, exactly as Amendment 1 argued.

**The per-line cost, and where it is spent.** `controller1` served 1,580 of the
2,002 lines and did so on **one backend pid** over `04:49:19.157 – 04:49:27.530`
= **8.373 s** — 3,160 statements through a single connection, **2.65 ms per
line**. nginx round-robined the agent's handful of bulk requests, so the split
was 1580 / 1 / 421 across the three replicas: **a single run's log stream is
sharded across every replica**, one bulk request at a time.

---

## Part B — the subscriber multiplier. `2N + 2NS` measured exactly, four times

All arms: 10 SSE streams' worth of `ssehold` against `controller3` on `:18083`,
never the LB. Every stream in every arm returned **200**, `diedEarly=0`,
`aliveAtEnd=S`, and **exactly 2002 events each**.

| S | predicted `2N + 2NS` | `taillogs_sse` measured | `2002 × S` | log-path total measured | window total | LISTEN backends |
|--:|--:|--:|--:|--:|--:|--:|
| 0 | 4,004 | 0 | 0 | 4,036 | 15,251 / 150 s | 0 |
| 1 | 8,008 | **2,002** | 2,002 | 8,042 | 19,142 / 150 s | 1 |
| 5 | 24,024 | **10,010** | 10,010 | 24,070 | 41,726 / 240 s | 5 |
| 10 | 44,044 | **20,020** | 20,020 | 44,105 | 61,939 / 240 s | 10 |

**The fan-out term is exact, not approximate: `2002 × S` to the statement in
every arm**, with the paired `GetRun` matching it on the same replica
(`getrun` on `controller3`: 2,013 / 10,024 / 20,038 against baselines of 9–11 on
the other two). The residue between measured and predicted (32 / 34 / 46 / 61)
is fully accounted for: `G`, the SSE backfill `TailLogsRecent` (exactly S per
arm), the archiver's one whole-log read, the S `LISTEN`s, and the driver's own
polling.

**Amendment 2 confirmed by effect.** `w6-pgsample.sh` reports
`controller3 listen idle peak=10` — one `listen`-class backend per stream,
exactly — while the 20,020 `TailLogs` and 20,020 `GetRun`s came from **13
distinct backend pids whose retained statement is an ordinary `logs`/`runs`
SELECT**, i.e. the `query` class (api + background, not separable), not
`listen`. The listen pool holds the subscription; **the API pool pays for every
wake.** This is the finding filed against `docs/operations.md:162`.

**The headline number.** On `controller3` during B-s10
(`w6-2a/B-s10/persec.txt`):

```
  05:05:12   11085 statements/s      <- peak
  05:05:11   10825
  05:05:13   10607
  05:05:10    9112
  05:05:14    1648
  ...  median over the 242-second window: 22 statements/s
```

**One run, ten viewers, four seconds above 9,000 statements/s against a resting
median of 22 on the same replica through the same instrument — ~504×.** The
2,002-line burst is 10 seconds of one shell loop.

### The reading this scenario got wrong first, and how it was caught

The first pass took `first .. last` spans per class and reported a **27-second
SSE backlog** — `taillogs_sse` last at `04:55:00.508` against the last
`INSERT` at `04:54:33.262` in B-s1. **It is not there.** The fixture's final
line, `burst-end`, is emitted after a 30 s sleep, so any span taken over the
whole run reads as a backlog that is an artefact of the fixture. The
per-second histogram (`w6-2a/*/timeline.txt`) shows the truth:

```
B-s10   second      insert  notify  taillogs
        05:05:10       452     452      4510
        05:05:11       491     491      4910
        05:05:12       503     502      5030
        05:05:13       481     481      4810
        05:05:14        73      74       740
        05:05:40         1       1        10     <- burst-end, 26 s later
```

**The fan-out lands in the same second as the ingest, at exactly 10× it, in
every arm.** The SSE path keeps up; what it costs to keep up is the finding.
Recorded because the span reading survived a first review and would have been a
fabricated defect. *Rule for later W6 arms: with `logburst`, read the
histogram, never the span.*

### The connection budget, watched as the brief instructed

Backends in `datname='unified'`, from the driver's own probes:

```
floor  69 -> 74      A-s0  74 -> 75      B-s1  76 -> 84
B-s5   84 -> 88 (subscribe) -> 90        B-s10 90 -> 95 (subscribe) -> 95
```

**Monotone and not released within this ~45-minute session**, in which the stack
was never restarted. **Stated at exactly that strength, and deliberately not as
"never released":** the window was ended by this scenario, it contained no
30-minute idle period, and the mechanism is the pgxpool one corrected further
down — connections are reclaimed on a 30 m idle / 1 h lifetime horizon that this
session never reached, so the series is consistent with the correction and does
not test it. What drives saturation is non-release **promptly**, which is what
`FINDINGS.md:2517` records. The
10 SSE streams added exactly 10 `listen` backends on `controller3` and the
client-backend total reached **96 of 100** with 3 superuser-reserved, i.e.
**one free non-superuser slot**.

**Task 1's saturation was not reproduced, and this arm is not evidence against
it.** After B-s10: `/readyz` and `/healthz` **200** on all three replicas
directly; 10 sequential authenticated reads through the LB **200**; 5 against
each controller directly **200**; **zero 401s**
(`w6-2a/post-bs10-auth.txt`). A first read of this check saw ten `400`s and
they were not a symptom — `GET /api/v1/runs` answers
`jobName query parameter is required`. **Cite `FINDINGS.md:2517`; nothing here
re-files it or contradicts it.** The difference is shape: Task 1's trigger was
**~2,554 req/s held at 8 in flight** — a *rate*, which `FINDINGS.md:2535` is
explicit about — and this is one run's serial write path plus ten long-lived
readers. No arm here came anywhere near that rate (the busiest second measured
11,085 statements/s at Postgres, from a handful of requests), which is why
non-reproduction here is expected rather than informative.

---

## Part C — what actually bounds a log-bulk request

Driven through the product's own routes by `tools/w6/w6-synth-agent.sh` against
`workloads/w6-probe` (`w6-2a/C/`), so every request carried a real agent
credential and passed the real `agentRunGuard`.

### C1 — bytes: the LB bounds them, the controller does not

Same credential, same route, same body; one 1-element batch whose single `line`
is the size under test, so request size is decoupled from the `2N`
amplification.

| body | via nginx `:18080` | direct to `controller1` `:18081` |
|---|---|---|
| 1 MiB line (1,048,719 B) | **413** in 0.002 s, 0 bytes uploaded | **204** in 0.026 s |
| 2 MiB line | **413** | **204** in 0.046 s |
| 8 MiB line | **413** | **204** in 0.156 s |
| 64 MiB line (67,109,007 B) | **413** | **204** in 1.262 s |
| 256 MiB line (268,435,599 B) | not attempted | **204** in **7.915 s** |

All five landed: `SELECT sum(length(line))` on the probe run returns
**347,078,656** bytes across 5 rows — 1+2+8+64+256 MiB, stored.

**The 256 MiB request moved `controller1`'s container memory from 421.1 MiB to
1.545 GiB — a ~1.12 GiB rise, ≈4.4× the body — and it did NOT come back down
inside the sampled window.** The metric is `docker stats` MemUsage, i.e. the
cgroup's `memory.current − inactive_file`; it includes kernel and socket memory
and is **not** process RSS, and an earlier version of this section called it RSS.
The multiplier is unaffected — it is arithmetic over two archived readings.
`w6-2a/C/bigbody-256m.txt`'s own `=== controller1 memory AFTER ===` reads
**1.541 GiB**, and `w6-2a/C/mem-during.txt` (3,908 lines) runs
421.1 MiB → 458.9 MiB → 807.3 MiB → 1.545 GiB → **1.541/1.54 GiB across 108
consecutive samples**, with `421.1MiB` appearing **exactly twice in the whole
file, both before the request**. **"It fell back to 421 MiB afterwards" is
retracted**: that reading came from the capture's `peak seen in the sampled
stream` block, which is a **deduplicated, non-chronological** listing, so its
421.1 MiB is the *pre*-request value. Retained rather than transient is the
stronger fact and it is what the filed entry's severity leg (2) now rests on.
Nothing in `manifests/` sets a memory limit on the
controller container (`grep -rn memory manifests/` returns **2** hits, neither a
container limit).

The LB's 413 is `FINDINGS.md:1655`'s cap, and is **cited, not re-filed**. One
detail it is worth adding: the `logfault` access log records these as
`413 arm= target= ... reqlen=349`, i.e. `arm=` empty and `reqlen` the *header*
length — nginx rejects an oversize body before the location that stamps
`$logfault_arm`, exactly as `README.md` records.

### C2 — cost: the bound that exists bounds the wrong quantity

`handleAgentLogBulk`'s cost scales with the **line count**, and the only bound
in the reference deployment caps **bytes**. So the question is not how large a
body gets through but how many round trips fit inside one the LB accepts.

| request | bytes | vs nginx 1 MiB | result | statements |
|---|--:|---|---|--:|
| 7,500 minimal lines, **via nginx** | 1,033,891 | 14,685 under | **204**, **9.499 s** | 15,000 |
| 7,600 minimal lines, **via nginx** | 1,047,691 | **885 under** | **204**, **9.356 s** | 15,200 |
| 20,000 minimal lines, **direct** | 2,768,891 | n/a | **204**, **24.637 s** | 40,000 |

Confirmed in the same window's statement log (`w6-2a/C2b/breakdown.txt`):
**35,100 `INSERT INTO logs` + 35,100 `SELECT pg_notify` = 70,200 = 2N exactly**,
`G = 2`, and `append_insert controller3 n=7500 pids=1` spanning
`05:18:34.284 – 05:18:43.757` — **one HTTP request holding one Postgres backend
per statement class for 9.473 s, across 15,000 statements**. The breakdown
prints `append_insert … pids=1` and `append_notify … pids=1` as **two separate
counts** and never prints the pid, so "one shared backend for all 15,000" is
inference, not measurement; it is almost certainly the same connection (insert
and `pg_notify` are consecutive calls inside one `AppendLog`) and printing the
pid is the one-line analyser change that would settle it.

**The per-line rate is flat at ~812 lines/s — 1.23 ms/line, 0.62 ms/statement —
on both paths**, so nginx contributes nothing to the cost. The 1 MiB cap does
not bound it because 885 bytes of headroom is still 15,200 statements.

### C3 — the enumeration of absent bounds, re-run as instructed

`grep -rn "MaxBytesReader" --include="*.go" .` returns **exactly 1 hit**:
`internal/controller/api_webhooks.go:118`. The brief's claim holds and the count
is reported, not truncated (`w6-2a/codesurvey.txt`).

Three further bounds are also absent on this path, enumerated at HEAD:
no `ReadTimeout`/`WriteTimeout` on the controller's `http.Server`
(`cmd/controller/main.go:451-455` sets `ReadHeaderTimeout` and nothing else);
no body-limiting middleware in the chain (`server.go:285-293` plus `ServerAuth`
and `auditLogMiddleware`); no `io.LimitReader` in the handler
(`api_agent.go:699` decodes `r.Body` directly).

**The asymmetry is the point.** The webhook route has a *configurable* cap —
`--webhook-max-body-bytes` / `UNIFIED_WEBHOOK_MAX_BODY_BYTES`, default 1 MiB
(`cmd/controller/main.go:209`), whose own flag help explains that oversize
bodies are "rejected with 413 instead of being truncated". The agent log-bulk
route has no cap, no flag and no env var. *(Incidentally, and not filed:
`grep -rn "WEBHOOK_MAX_BODY" docs/` outside `docs/superpowers/` returns **0**
hits — the webhook knob is undocumented too. Out of this scenario's scope.)*

### C4 — the seal path, met by accident and cited not re-filed

The first C2 attempt pushed 20,000 lines at a run the stuck-run reaper had
failed and the archiver had sealed while the probe was being built. The
controller answered **204** and stored **nothing**; the only trace is a
controller-side `WARN`:

```
{"level":"WARN","msg":"dropping log lines for sealed run","run":"d86f08be-…","dropped":20000}
```

That is `AppendLog`'s documented seal behaviour (`postgres.go:911-917`) reaching
`handleAgentLogBulk`'s `dropped` counter (`api_agent.go:730-737`), and it is
W3-5's territory. **Cited, not re-filed.** It is recorded because it cost this
scenario a repeat arm and will cost the next one the same: a synthetic agent's
run goes stale in ~90 s, and a sealed run accepts pushes with a 204.

---

## Part D — the NOTIFY consumers, enumerated from the captures

`w6-2a/partD-enumeration.txt`, across all seven windows (~2.1 M raw lines).
**Enumerated from the evidence, with the grep as the cross-check rather than the
source** — the wave's standing warning is that a class gets declared fully
enumerated while a third producer sits unevaluated inside its own capture.

- **Producers: one.** **49,114** `pg_notify` records, and **every single one**
  carries `$1 = 'log_appended:<runID>'`. No other channel argument appears in
  any capture. Code cross-check: `grep -rn "pg_notify" --include="*.go" .`
  returns **2** hits, one of which is a comment in this campaign's own
  `ssehold` source — **1 in product code**, `postgres.go:935`.
- **Consumers: one.** **18** `LISTEN` records, all
  `LISTEN "log_appended:<runID>"`. No `LISTEN` or `NOTIFY` statement of any
  other kind appears anywhere. Code cross-check: `ListenForNotify` has **1**
  product call site, `sse.go:118`.
- **`UNLISTEN`: zero records, in any capture.** This is not an absence of
  interest — it is the mechanism of D2 below.
- **Cost of one wake: exactly 2 API-pool statements** — `TailLogs(…, 10_000)`
  and `GetRun` — plus an unconditional `flusher.Flush()`, whether or not the
  wake produced a line.

### The three `TailLogs` callers share one SQL text and are separated only by `$3`

`postgres.go:939`'s statement is issued by `sse.go:120` (limit **10000**),
`archiver.go:81` (limit **1000000**) and `api_runs.go:221` (limit **1000**) with
byte-identical text; the pools differ (api / background / api) and Postgres
cannot see pools. `log_parameter_max_length` is `-1` on `postgres:16-alpine`, so
every `execute` record is followed by a `DETAIL:  parameters:` record on the
same backend pid and `$3` disambiguates all three. **Without this, the
archiver's one whole-log read per run is counted as an SSE wake** — which is
what the first version of `breakdown.py` did, reporting `taillogs_sse = 1` in
the S=0 arm.

**And the CLI is not an SSE consumer at all.** `unified-cli logs -f` and
`run --follow` poll `GET /api/v1/runs/{id}/logs?after=N` every 300 ms
(`internal/cli/wait.go:134-137`, `:185`) — a *separate* amplifier on the same
table, on the API pool, with no NOTIFY involvement. The only product SSE
consumer is the web UI (`web/src/routes/RunDetail.svelte:841`).

### D2 — a released listen-pool connection keeps its LISTEN registrations

**Code read that motivated the experiment.** `ListenForNotify`
(`postgres.go:1665-1677`) acquires from `listenPool`, issues `LISTEN`, and
`defer conn.Release()` — **never `UNLISTEN`**. `pgxpool.Conn.Release`
(pgx v5.9.2, `pgxpool/conn.go:20-67`) destroys the connection only if it is
closed, busy, or in a transaction, or past its lifetime, or if an
`AfterRelease` hook says so; otherwise it is a bare `res.Release()` — **no
`DISCARD ALL`, no reset of any kind**. `newPostgresPool` (`postgres.go:90-103`)
sets only `MaxConns`, so `afterRelease` is nil and the bare branch is the one
taken.

**Measured** (`w6-2a/D/stale-listen-analysis.txt`):

```
ts=05:24:27.662 pid=4560 host=controller3 chan="log_appended:dab6e159-…"   <- run A's stream
ts=05:24:39.477 pid=4560 host=controller3 chan="log_appended:54e9b7f6-…"   <- run B's stream
```

*(Two substitutions in that block, both disclosed: run ids elided at the `…`,
and `host=` rewritten from the capture's `172.20.0.5` to the service name it
maps to — `w6-2a/D/w6-idleload-D2-ipmap.txt` reads `controller3 172.20.0.5`.
Nothing else is altered.)*

**The same backend, pid 4560**, twelve seconds and one closed stream apart. Then
50 lines were appended to **run A only**:

```
ts=05:24:45.930 pid=146 run=runB afterSeq=0 limit=10000
…  50 records, 05:24:45.930 – 05:24:45.993   (49 on pid 146 to .992; the 50th at .993 on pid 142)
ts=05:24:51.506 pid=141 run=runB afterSeq=0     limit=10000   <- the control begins
ts=05:24:51.507 pid=141 run=runB afterSeq=43164 limit=10000
…  10 records with advancing afterSeq
```

**Run B's subscriber issued 50 `TailLogs` and 50 `GetRun` for a run it is not
subscribed to**, all at `afterSeq=0`, returning nothing, inside **63 ms** — and
`ssehold` records **`firstEvent_ms = 12049.9`** for that stream, i.e. its client
received **zero** events until the control push 6 s later. The control proves
the stream was live: 10 lines to run B produced 10 wakes with advancing
`afterSeq` and 10 delivered events.

**One subscriber, 120 queries where 20 were warranted — 6×.** The amplification
formula is therefore not `2N + 2NS` but `2N + 2N·(streams on connections that
carry this run's channel)`, a set that includes every *former* subscriber's
connection since reused.

**It is bounded, and the bound is a pgx default the product never chose.**
`MaxConnLifetime` = 1 h and `MaxConnIdleTime` = 30 min (pgx v5.9.2,
`pgxpool/pool.go:22-23`) destroy a connection eventually, so a connection
accumulates channels for at most an hour of subscription churn rather than for
the process lifetime. `newPostgresPool` sets neither.

### D3 — an SSE stream is never closed when its run terminates mid-stream

`sse.go:106-111`: if the run is **already terminal at connect**, the handler
writes one `status` event and **returns** — the stream closes. `sse.go:138-142`:
if the run becomes terminal **during** the stream, the callback writes the same
`status` event and **does not return**, so `ListenForNotify` goes straight back
to `WaitForNotification`. Measured in every B arm: the run reached `Succeeded`
at ~`05:05:41` and `ssehold` reported `aliveAtEnd=10, diedEarly=0` at
`05:07:31` — **10 listen-pool backends held for 110 s past the run's terminal
state**, ended by the client, not the server. *(No claim is made about what
happens at longer horizons: that window was ended deliberately.)*

---

## Findings filed

| # | `FINDINGS.md` | Kind | Severity | Subject |
|---|---|---|---|---|
| 1 | `:2539` | violation (documented contract) | minor (docs gap) | `docs/operations.md:162` promises SSE listeners cannot consume API capacity; every wake spends two API-pool connections |
| 2 | `:2562` | observation | minor | a released listen-pool connection keeps its `LISTEN`s, so a subscriber is woken by runs it never subscribed to |
| 3 | `:2584` | observation | major | nothing bounds a log-bulk request on the direct path, and the bound that exists caps bytes while the cost scales with line count |
| 4 | `:2614` | observation | minor | an SSE stream whose run terminates mid-stream is never closed by the server |
| 5 | `:2622` | observation (campaign asset) | minor | `w6-idleload.sh` did not stop its own capture; three finished captures kept growing |

**Entry 2 was first filed `major` and is now `minor`, re-argued at review from
the band's own text rather than by analogy.** `FINDINGS.md:6-8` defines major as
"incorrect visible behavior, unbounded recovery"; the stale-`LISTEN` waste
satisfies **neither** limb — nothing visible is incorrect (zero rows, zero
events, every line still delivered) and the recovery is bounded by pgxpool's
30 m / 1 h defaults, a bound the entry states itself. Invisibility **aggravates**
the finding — it is why nobody can budget for the term or find it afterwards —
but that is an argument about diagnosability, which is the minor band's own
subject. It now sits alongside entries 1 and 4, which are the same class of
invisible cost and are already minor.

**A scoping limit on `FINDINGS.md:2517` that two of the severity arguments here
leaned on, discovered at review and now stated in all three entries.** `:2517`'s
"the shipped pool sizing over-commits this Postgres by roughly 9×" is true of
the **test rig** and does not generalize: `test/ha` runs stock
`max_connections=100`, but the repository's own `docker-compose.yaml:30` starts
Postgres with `max_connections=1000` and `docs/operations.md:173` tells operators
so — 3 × 304 = 912 < 1000. Entries 2 and 4 both reached for "the API pool
saturates first" / "the resource is scarce"; both now say which deployment that
is true of. Nothing measured changes; the blast radius does.

Three things this scenario **cited rather than re-filed**, per the standing rule:
`FINDINGS.md:1521` (W3-4, the same loop's duplication under retry),
`FINDINGS.md:1655` (nginx's 1 MiB default), and `FINDINGS.md:2517` (Task 1's
connection-pressure observation — whose saturation this scenario did **not**
reproduce, and does not contradict).

One correction supplied to an inherited fact, recorded in
`README.md` §"The idle floor": **"the pools only grow" is not what the code
says.** `newPostgresPool` sets no `MaxConnIdleTime`/`MaxConnLifetime`, but
pgxpool applies defaults when they are unset — `MaxConnLifetime = 1h`,
`MaxConnIdleTime = 30m` (pgx v5.9.2, `pgxpool/pool.go:22-23`, from `ParseConfig`
at `:417-430`). The code read supports "not returned promptly, reclaimed on a
30-minute idle horizon", not unbounded accumulation. This scenario's own
backend series (69 → 74 → 75 → 76 → 84 → 90 → 95 → 93 over ~45 minutes of
**continuous** use) neither tests nor contradicts that.

**No invariant is filed against.** I4 was checked in every arm and held
(2002 rows, contiguous `seq`, zero duplicate `(line, ts)` groups, `Succeeded`).
I5 is not reachable — no fault was injected and no recovery path was exercised.
The `2N + 2NS` prediction was **confirmed to the statement**, which is a
confirmed prediction about a defect and not a contract, so it is reported as a
measurement rather than filed.
