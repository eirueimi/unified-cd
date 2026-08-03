# W6-1 — connection pressure: what breaks first, and what actually drives it

**Charter: find the first breaking point and its cause. This scenario does NOT
produce a sizing number, and the refusal is deliberate.** Everything below runs
on one Windows laptop against a three-controller `test/ha` stack whose Postgres
is at the stock `max_connections=100`. A "supports ~N concurrent users" figure
derived from that is an extrapolation across a different `max_connections`, a
different CPU, a different network and a different working set, and an operator
who reads one will size a deployment with it. What this scenario can honestly
produce is **which resource runs out first, which knob moves it, and how the
exhaustion presents on the surfaces an operator is told to watch** — and those
transfer. The absolute numbers do not. Where a number appears below it is
labelled with the rig it was taken on.

**The answers, up front.** The first breaking point is **Postgres's
`max_connections`**, reached through the **api** pool rather than the listen
pool. **Concurrency at a latency-bearing endpoint is by far the cheaper route to
it**: 8 workers at 20 req/s do not saturate, while **60 concurrent agent claim
long-polls at three requests per second** do — every one of them answered `200`.
Rate is a real second route and costs ~820× the requests (8 workers at
2,451 req/s also saturates). **What is NOT claimed, after review: that
concurrency alone is the driver.** The cheap arm changes endpoint as well as
concurrency, the one within-endpoint comparison is confounded by burst width and
by a contaminated floor, and the clean single-variable isolation was not run —
see Part B, which states this at length and names the arms that would settle it.
When
it happens, **nothing reports it**: `/readyz` reads 200 in 143 of 144 saturated
in-window replica-readings (47 of 48 sample instants), the controllers log
nothing, and the first visible symptom is
401s on a valid admin token. **No sizing number appears below; see the charter.**

**Invariants attacked:** I5 and I7.

- **I5** (`docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:52`),
  verbatim: *"**Bounded recovery** — after fault injection the system returns to
  steady state within documented bounds (leader re-election ≤ seconds; stuck-run
  reap ≤ staleAfter 90s + interval 30s; the bounds in `docs/high-availability.md`
  are the contract)"*. Part D is where this has its best shot, and **only if a
  documented bound is missed** — the campaign rule at `FINDINGS.md:1509` forbids
  reaching for an invariant that the spec's own text does not cover.
- **I7** (`:54`), verbatim: *"**State display consistency** — run status, approval
  status, and audit rows never contradict each other or reality"*. Part C tests
  whether the **health** surface contradicts reality; whether `/readyz` counts as
  a "state display" is arguable, so the health limb below is argued on the
  **documented-contract** limb primarily and I7 only secondarily.

The table is at `:48-54`; I1 is `:48`, I4 is `:51`.

---

## What this scenario inherits and must NOT re-derive or re-file

- **`FINDINGS.md:2517`** (W6-infra observation, W6 Task 1). A sustained
  **~2,554 req/s held at 8 in flight** pinned Postgres at `max_connections`,
  `/readyz` stayed **200** on all three replicas at the one point it was sampled,
  and a valid admin PAT returned **401** on about a third of requests. **Cite it;
  do not re-file it.** This scenario extends it in exactly the two places it
  invites: the rate/concurrency separation it says it does not make, and the SSE
  question it explicitly hands over.
- **`FINDINGS.md:43`** (W0-1). Controllers crash-loop with no DB-connect retry at
  startup. **Cite; do not re-file** when Part D triggers it.
- **The idle floor** (`README.md` §"The idle floor"): ~71.8-72.0 q/s across three
  controllers and two agents with zero runs; `ClaimNextRun` is ~49 % of it;
  **73-74 of 100 backends at rest**, i.e. ~26 free slots. Two independent
  measurements 0.4 % apart.
- **pgxpool applies `MaxConnLifetime=1h` / `MaxConnIdleTime=30m` when unset**
  (pgx v5.9.2, `pgxpool/pool.go:22-23`). "The pools only grow" is false and was
  corrected; non-release **promptly** is what drives saturation.
- **A stale `LISTEN` is never `UNLISTEN`ed** (`internal/store/postgres.go:1665-1682`,
  `sse.go:118`), and pgxpool's `Release` does no reset. Filed in W6-2a
  (`FINDINGS.md:2562`).
- **The 9× over-commit is scoped to this rig.** `test/ha` runs stock
  `max_connections=100`; the repository's own `docker-compose.yaml:30` starts
  Postgres with `max_connections=1000` and `docs/operations.md:173` tells
  operators so. 3 × 304 = 912 < 1000. **Every claim below about a real deployment
  says which of the two it means.**

---

## Corrections to inherited facts, established BEFORE execution

Six consecutive waves have had a plan's "verified code facts" corrected by
execution, and the pattern is that `file:line` claims hold while **mechanism**
claims fail. Both classes were re-checked here before a single arm ran.

### The `file:line` claims — every one holds

| Claim | Verified |
|---|---|
| `newPostgresPool` sets only `MaxConns` | `internal/store/postgres.go:88-102`. `cfg.MaxConns = maxConns` is the **only** field assigned after `ParseConfig`. HOLDS |
| Four pools, 128/32/16/128 | `postgres.go:68-71` (the `specs` slice) with the values at `postgres.go:46-53` (`DefaultPostgresPoolConfig`). 304/replica, 912 across three. HOLDS |
| `sse.go:69-103` backfills before `:118`'s `ListenForNotify` | `:69` `TailLogsRecent`, `:103` `flusher.Flush()`, `:118` `_ = s.store.ListenForNotify(...)`. HOLDS, and the `_ =` is verbatim |
| `ListenForNotify` blocks on `conn.Acquire` | `postgres.go:1665-1667`: `conn, err := p.listenPool.Acquire(ctx); if err != nil { return err }`. HOLDS |
| `auth.go:79` fires an undeadlined `TouchPAT` goroutine per authenticated request | `internal/controller/auth.go:77-79`, verbatim `go func() { _ = st.TouchPAT(context.Background(), pat.ID) }()`. HOLDS |
| `docs/operations.md:154`, `:162`, `:173`; `docs/high-availability.md:241`, `:289` | all five quoted verbatim below from the files. HOLD |

### AMENDMENT 1 — the experiment `FINDINGS.md:2535` proposes cannot be run, because in-flight is not a control variable

`:2535` says separating rate from concurrency is *"a one-command experiment
(`-c 8` with a per-worker delay, so 8 in flight at ~50 req/s)"*. **It is not, and
the reason is arithmetic rather than tooling.** By Little's Law,

```
in-flight  =  request rate  ×  per-request latency
```

so *8 in flight at 50 req/s* requires **160 ms** of server latency per request.
The endpoint `:2517` used — `GET /api/v1/runs?jobName=...` against an empty job —
answers in about **2-3 ms** on this rig. Pacing the same 8 workers down to
50 req/s therefore holds **~0.15** in flight, not 8. **In-flight is an OUTCOME of
rate and latency; the only two things a client can set are the worker count and
the pacing.** Any experiment that claims to hold N in flight while varying the
rate by 50× is either using a latency-bearing endpoint or is mis-stating what it
did.

This has a consequence for the design below, and it is the honest version of
Part B: the four arms vary **worker count** and **rate** independently, and the
"high concurrency at near-zero rate" corner is reached with a **latency-bearing**
endpoint (SSE, which holds for its whole life at zero request rate) rather than
by pretending a paced GET can do it. The two corners are on different endpoints
and therefore different pools — **stated as a limitation, not hidden**, and Part A
is what makes the listen-pool corner interpretable on its own.

`loadgen` had no pacing flag at all, so `:2535`'s "one command" did not exist
either. One was added (see Harness changes).

### AMENDMENT 2 — `/readyz` has TWO distinct ways to fail and they respond to different variables

`docs/operations.md:154` says `/readyz` *"also acquires an API-pool connection
and pings PostgreSQL"*. The implementation (`internal/controller/server.go:318-332`)
calls `s.store.Ping` under a **3 s** `context.WithTimeout`, and `Ping` is
`postgres.go:145-147` — `p.pool.Ping(ctx)`, i.e. **the api pool**. `pgxpool.Ping`
acquires a connection and pings on it. So:

- **Under RATE saturation** — Postgres at `max_connections`, but the api pool
  already holds open, idle connections — `Acquire` returns an **existing** conn
  and the ping succeeds. **`/readyz` reads 200 while Postgres refuses every new
  connection.** This is the `:2517` observation, and it is not a bug in `/readyz`
  so much as a statement that `/readyz` measures the pool, not the server.
- **Under CONCURRENCY saturation** — every one of the 128 api-pool connections
  checked out by an in-flight handler — `Acquire` **blocks**, the 3 s timeout
  fires, and `/readyz` returns **503 db unavailable**.

**Prediction stated before execution: `/readyz`'s failure is driven by
concurrency, not by Postgres exhaustion, and the two can be pulled apart by the
same arms Part B uses.** If that holds it sharpens `:2517`'s weakest leg rather
than just repeating it.

### AMENDMENT 3 — the arithmetic says the listen path needs ~26 streams, not ~100, and it says so on THIS rig only

The wave plan assumed ~100 concurrent SSE streams to saturate, reasoning from
the 128-connection listen pool. With 73-74 backends at rest out of
`max_connections=100` and `superuser_reserved_connections=3`, the free
non-superuser budget at rest is **97 − 73 ≈ 24 slots**, not 128. **Predicted
first breaking point by the listen path: ~24-26 streams.** `:2535` already made
this correction in prose; Part A measures it.

**And it is a rig property.** On the reference compose (`max_connections=1000`)
the same 128-connection listen pool is inside budget and the listen path does not
reach a server-wide limit at all — it reaches its own pool cap first.

---

## The rig

```bash
cd test/ha
export COMPOSE_FILES="-f docker-compose.ha.yaml \
  -f ../edgecase/compose/ctrlports.override.yaml"
docker compose $COMPOSE_FILES up -d --build
```

`logfault.override.yaml` is deliberately **not** stacked: every arm here
addresses a controller port directly, so nginx's access log records nothing this
scenario reads, and leaving nginx stock keeps the rig one overlay simpler.

`ctrlports.override.yaml` is required — every rate-bearing arm addresses
**controller1 on :18081** directly. Measuring through the LB measures the LB
(`test/ha/nginx.conf` has no upstream `keepalive`, leaves `worker_connections` at
512, and `proxy_next_upstream_tries 3` can turn one client request into three).

**Overlay armed-check before any arm:** `/healthz` on 18081/18082/18083 all
answer, and the three are three different processes
(`README.md` §"The W6 harnesses": `metrics_families=128 / 112 / 113`).

### Harness changes

**`loadgen` gains `-delay`** — a per-worker sleep between requests in
`-mode sustained`. It lowers the **rate** at a fixed **worker count**; per
Amendment 1 it does *not* lower in-flight at a fixed rate, and the flag's own
documentation says so at the point of use. Because a paced run leaves its
workers idle on purpose, `maxInFlight` is *expected* far below `-c`, so the
**under-report** guard is suppressed and replaced by an explicit `PACED:` line;
the **over-report** guard — the one that caught a fabricated number and reached a
FINDINGS entry — is untouched, because it is still arithmetically impossible and
still always an instrument fault. `go test ./test/edgecase/tools/w6/loadgen/`
still passes.

**Arm verification for `-delay`, before any measurement used it:** `-c 4
-duration 4s -delay 200ms` produced **80 requests in 4.000 s = 20 req/s,
meanInFlight=0.04**; the same command without `-delay` for 2 s produced **4,444
requests in 2.002 s = 2,220 req/s, meanInFlight=3.99**. Same worker count, a
**111×** rate difference, and the in-flight figure moved with the rate exactly as
Little's Law says it must. The flag is verified by its effect.

### Instruments

- `tools/w6/bin/loadgen` — worker count and rate, per-request CSV, `maxInFlight`
  guarded in both directions.
- `tools/w6/bin/ssehold` — S concurrent SSE streams held for a window;
  `aliveAtEnd` / `diedEarly`, per-stream status and event counts. **This is the
  only instrument that can hold high concurrency at zero request rate.**
- `tools/w6/w6-pgsample.sh` — `pg_stat_activity` on a grid, per replica and per
  derived pool, **with `-p` probing `/readyz` and `/healthz` inside the same loop
  iteration.** `-p` exists precisely so this scenario gets the in-window health
  reading `:2517` could not.
- `tools/w6/w6-synth-agent.sh` — synthetic agent identities for Part E, and the
  owner of the long-lived `edge-w6-probe` run the SSE arms subscribe to.

**Two harness caveats, both of which have already bitten:**

1. `w6-pgsample.sh`'s `TOTAL backends ... of max_connections` line **counts
   Postgres background workers**, which carry a NULL `datname` and consume no
   `max_connections` slot. Every saturation claim below is made against **client
   backends** (`datname IS NOT NULL`) compared to
   `max_connections − superuser_reserved_connections`.
2. The file-descriptor leak that made a "finished" capture keep growing is fixed
   in `w6-idleload.sh` and `w6-reqshape.sh`. Neither is used here — no arm below
   captures container logs to a file in the background — and the two tools that
   are used (`w6-pgsample.sh`, `loadgen`) are bounded by their own `-d` /
   `-duration` and exit on their own. **How that was verified is recorded in the
   results**: every capture's row count is checked against its own nominal grid.

---

## Parts, with predictions stated before the first capture

### Part A — establish the ceiling empirically

Measure, not inherit: `max_connections`, `superuser_reserved_connections`, the
per-replica pool caps, and **how many concurrent SSE streams it actually takes**
to reach the ceiling.

**Predicted:** `max_connections=100`, `superuser_reserved_connections=3`, pools
128/32/16/128 = 304 per replica. Free non-superuser budget at rest ≈ 97 − 73 = 24.
**~24-26 streams reach the ceiling by the listen path — an order of magnitude
below the 100 the wave plan assumed.**

### Part B — separate rate from concurrency

Four arms. Per Amendment 1 the controls are **worker count** and **rate**; the
zero-rate/high-concurrency corner needs a latency-bearing endpoint.

| Arm | Workers | Rate | Endpoint | Purpose |
|---|---|---|---|---|
| B1 | 8 | uncapped (~2,500/s) | fast GET | reproduce `:2517` |
| B2 | 8 | ~20/s (`-delay 400ms`) | fast GET | **same workers, ~125× less rate** |
| B3 | 200 | ~20/s (`-delay 10s`) | fast GET | **25× the workers, same rate as B2** |
| B4 | 200 | uncapped | fast GET | both high |
| B5 | 40 streams | ~0/s | SSE | **pure concurrency, zero rate** (Part A's instrument) |

**Predicted:** B1 and B4 saturate; B2 does not; B3 does not. If that is what
happens, **rate drives api-pool saturation and worker count does not** — which
would settle `:2517`'s open question in the direction its own amplifier note
expects. B5 saturates by a **different** path (the listen pool), so the complete
answer is expected to be "rate drives one path, concurrency drives the other, and
they share one `max_connections`."

### Part C — what breaks first, and how it presents

- **`/readyz` in-window**, via `w6-pgsample.sh -p 18081,18082,18083`, on the same
  grid as the backend count. Per Amendment 2, predicted **200 under B1 and 503
  under B4**.
- **Do background jobs starve before the API does?** The background pool is 32
  against the api pool's 128. Read controller logs for scheduler / reaper /
  archiver errors inside each window.
- **Settle the open SSE question** (`:2537`, handed here explicitly): does a
  client past the cap get a **200 with backfill and then a silently dead
  stream**? From the code: the response headers and the whole backfill
  (`sse.go:69-103`) are written **before** `:118` acquires a listen connection,
  so a client past the cap must see **200 + backfill**; `ListenForNotify` then
  returns an error which `_ =` discards, `handleRunEvents` returns, and the
  response body **ends**. **Predicted: 200, full backfill, then the stream is
  CLOSED — not silently dead but silently EMPTY, and the distinction between
  those two is the thing to capture.** `ssehold` reports `diedEarly` and per-stream
  event counts, so both are measurable.

### Part D — the recovery limb, and the dangerous one

Restart a controller while Postgres is at `max_connections`. `FINDINGS.md:43`
records controllers crash-looping with no DB-connect retry at startup, and
**`test/ha/docker-compose.ha.yaml` sets no `restart:` policy on the controllers**,
so a controller that exits stays exited. Measure whether the stack recovers on
its own and for how long it is degraded.

**Predicted:** the restarted controller fails `store init` and exits(1); with no
`restart:` policy it does not come back; the stack runs two-of-three
indefinitely. **I5 is reachable only if a bound `docs/high-availability.md`
documents is missed** — the spec names leader re-election and the stuck-run reap.
A replica that is *down* is not a bound miss by itself; a **leader** that is down
and not replaced within seconds is.

### Part E — the claim long-poll surface

`ClaimNextRun` is **49 %** of the idle floor from just two agents. Add synthetic
agent identities with `w6-synth-agent.sh` and measure what N concurrent
long-polls cost in connections and in statements.

**Predicted:** each long-polling agent holds a backend for the poll's duration,
so N agents cost ~N backends plus the per-poll statement rate; the marginal cost
per agent is predicted at roughly the measured 17.6 q/s per agent
(35.25 q/s ÷ 2). Whether the long-poll holds a connection **continuously** or
releases between polls is the thing to measure, and it decides whether agents
scale like SSE streams or like ordinary API clients.

---

## RESULTS

Raw evidence: `edgecase-evidence/w6/w6-1s/` — **54 files at top level plus a
`void/` subtree of 46, 100 in total** (recounted at review, after the orphan
driver was moved into `void/` and two files were added; see I4 and M5 below).
Rig: `test/ha` +
`compose/ctrlports.override.yaml`, `max_connections=100`,
`superuser_reserved_connections=3`, so the **client-backend budget is 97**. Every
saturation claim below is made against **client backends** (`datname IS NOT
NULL`) or against the strongest reading of all — `psql` itself refused.

### Three instrument defects found, and the third is the one that invalidated captures

**1. `w6-pgsample.sh` died of the thing it measures.** `set -euo pipefail` plus a
bare `rows=$(psql_ …)`: the first `FATAL: sorry, too many clients already` ended
the script. Part A's 40-stream arm asked for a 130 s grid and got **2 samples**
(`void/A2-partial-sweep/`, and `A3-pg.txt`), both from *before* the exhaustion —
and the `-p` health series, the entire reason `-p` exists, stopped with it. A
second copy of the same defect sat in the preamble, so a capture *started* into
an existing saturation never reached its own loop at all. Fixed with `psql_ok`
(records `unavailable`, keeps sampling) plus `PG_MAXCONN_`/`PG_RESERVED_`
overrides. **Verified by effect**: `harnessfix-pgsample.txt` — 5 of 5 samples
`unavailable`, health probed on every one, and the closing summary reporting
`UNAVAILABLE: 5 sample rows`. *A sampler that cannot outlive the fault is not an
instrument.*

**2. The same tool's saturation test was on the wrong side of its own README
caveat.** Its summary compared `total_backends` — which includes Postgres's
background workers — against `max_connections - 3`. It printed *"AT/NEAR
max_connections in 156 of 180 samples"* for a window whose client backends were
**93 of a 97-slot budget**, i.e. not saturated at all
(`void/A2-partial-sweep/A2-sweep.txt`). Now compares `db_backends` against
`max_connections - superuser_reserved`, and treats `unavailable` as saturated.

**3. `w6-synth-agent.sh heartbeat` terminalises the agent's own runs, and it cost
two whole captures.** The verb sent `-d '{}'`. `handleAgentHeartbeat` gates
reconcile on **body presence**, not on the decoded slice — verbatim,
`internal/controller/api_agent.go:88-91`: *"gated on BODY PRESENCE
(r.ContentLength != 0), not on the decoded slice being non-nil"* — so `{}` is
`ContentLength=2` and reports an **empty** active set, which is exactly the
"the agent restarted and forgot its runs" signal. A 25 s keepalive loop failed
the run it existed to protect, **4 s into the first arm**, and the arm then
measured an already-terminal run and reported `diedEarly=20/30/45/70` that had
nothing to do with connection pressure (`void/void-heartbeat-kill/`,
`void/void-run-failed-2/`). The verb now takes run ids. The product behaviour is
correct and documented in its own comment; the harness was wrong.

**And one driver defect of the campaign's recurring family, which contaminated
two captures and hid it by buffering.** A `nohup driver.sh &` launched from the
agent's shell **outlived the tool call by ~6 minutes**, and its own output file
showed only the first arm because stdout was block-buffered — so it looked dead.
It was in fact running a **200-worker max-rate arm** at 08:07:44 while a
"zero-load control" and a three-controller restart were taken on the same rig.
That control appeared to show a stack going from 23 to ≥97 client backends **in
12 s with no load**, which would have been a spectacular and completely false
finding; the orphan's own `B-B4-*` files, timestamped inside that window
(`void/void-B4-orphan/`), are what identified it. **Every arm below was re-run in
the foreground.** Same lesson as the two capture leaks the README records:
*the killed thing was not the thing holding the resource.*

**Two archival corrections to that paragraph, made at review.**

- **The orphan driver's own output was still sitting in the live archive**, under
  `B-results.txt` — the most inviting filename in the directory — while only its
  `B-B4-*` pg files had been moved. It runs B2/B3/B1/B4 from 08:01:14 to
  08:08:05, i.e. it **is** the contaminating window, and it carries four
  superseded arms whose numbers differ from the published ones (B3 floor 76 and
  100 after with **91 of the 100 on one replica**; an `11 of 12` readyz line for
  B1; a `readyz 000=1` triple discussed nowhere). Leaving it there contradicts
  this scenario's own stated corollary that any window-overlapping capture is
  void. It is now at **`void/void-B-orphan-driver/`** together with the driver
  itself (`B-driver.sh`) and a `NOTE.txt` that enumerates the superseded numbers
  and explains the `000` readings — `curl`'s no-response code, seen
  simultaneously on all three ports, i.e. a probe that lost its turn on a laptop
  running a second driver's 200-worker arm, not three replicas failing together.
  **No number in that file is published anywhere and none may be.**
- **The "23 → ≥97 in 12 s with no load" figure traces to no capture.** It was
  read live from the v1 zero-load control, and that control was re-run: the
  archived `B0-control.txt` is explicitly headed "B0 CONTROL **v2**" and v1 was
  overwritten. So the figure is **observed-live, uncaptured**, and it is labelled
  that way here rather than quietly carried — the same treatment
  `FINDINGS.md:2536` gives its own uncaptured `ssehold` observations. It is used
  only to describe what the contamination looked like, never as a measurement.

**M5 — the B-arm commands, which were recorded nowhere.** E-60's full command is
in its FINDINGS Repro line; B1-B4's were not in this runbook, in `B-B*.txt` or in
`B-results.txt` — only the flag string survived, without the URL or the headers.
The driver that produced the published arms is now archived as
**`B-arm.sh`** in the live evidence directory. From it, every B arm is
`tools/w6/bin/loadgen -url "http://localhost:18081/api/v1/runs?jobName=edge-tick"
-H "Authorization: Bearer ha-admin-token" -c <N> -mode sustained -duration <D>
[-delay <d>] -label <name> -out B-<name>-req.csv -error-bodies
B-<name>-errbodies.txt`, paired with `w6-pgsample.sh -i 2 -d <D+10> -p
18081,18082,18083` started 3 s ahead of it. `ha-admin-token` is `test/ha`'s
static fixture token, already published at `FINDINGS.md:2517`.

**Window-boundedness, as the brief required it to be stated.** No arm here uses
`docker compose logs -f` or any background capture. `w6-pgsample.sh` and
`loadgen` are each bounded by their own `-d` / `-duration` and exit on their own;
the check applied to every capture is that its row count matches its nominal
grid — e.g. `B0-control-pg.csv` reports `25 samples over 420s` at `-i 15`
(420/15 = 28 nominal, 25 actual: the shortfall is the `psql` round-trip inside
each iteration, which stretches the grid, and the printed timestamps show it —
17.2 s spacing, not 15 s). **Rates below therefore divide by the capture's own
printed span, never by the nominal one.**

---

## Part A — the ceiling, measured. Predicted ~24-26 streams; measured **24**

`A1-ceiling.txt`: `max_connections=100`, `superuser_reserved_connections=3`,
zero pool-cap overrides in `test/ha/docker-compose.ha.yaml`
(`grep -c 'MaxConns\|POOL'` = **0**), so the caps are the code defaults
`postgres.go:46-53` — api 128, background 32, lock 16, listen 128 = **304 per
replica, 912 across three, against 97 usable slots**.

**The single decisive arm** (`A3.txt`): from a freshly restarted stack settled to
**59** client backends, **40 concurrent SSE streams** on one live `Running` run,
opened against **one** replica, 250 ms stagger, 60 s hold:

```
requested=40 opened=40 hold=1m0s wall=1m0.001s
aliveAtEnd=24 diedEarly=16 non200=0 totalEvents=1880
status   200=40
```

**24 survive; the 25th onward do not.** The prediction from the arithmetic
(97 − 73 ≈ 24 free slots at the README's resting floor) was **~24-26**, and the
measurement is **24**. The wave plan's assumption of ~100 is wrong by a factor of
four in the direction `FINDINGS.md:2535` already suspected, and this is the
measurement behind that suspicion rather than another restatement of it.

Two things the arm adds beyond the number:

- **It is not one connection per stream.** 24 surviving streams consumed the
  whole 38-slot free budget, because each subscriber also spends **API-pool**
  connections per NOTIFY wake — the mechanism W6-2a filed at `FINDINGS.md:2539`
  — and the fixture emits one line per second, so 24 viewers cost 48 API queries
  per second on top of 24 `listen` backends. **The listen pool is not the whole
  cost of a subscriber and sizing from `ListenMaxConns` alone will under-count.**
- **Postgres was pinned hard enough to refuse `psql` on the unix socket**, which
  is a stronger reading than any count.

**Connections are not released promptly — and this one number comes out of a
VOIDED capture, so what is relied on is stated exactly.** In the earlier laddered
sweep (`void/A2-partial-sweep/A2-sweep.txt`) client backends went 73 → 83 at
S=10 → **93** at S=20 — exactly +20 for 20 streams — and **stayed at 93 for
about three minutes after those 20 streams closed** (the `db_backends=93` series
runs unbroken to sample 60 at 07:47:24).

**What that capture was voided for, and why the plateau nonetheless stands.** The
sweep is void because its later arms ran against a run the harness's own
`heartbeat` verb had already failed: at S=30/45/70 the run reads `Failed` and
`ssehold` reports `aliveAtEnd=0 diedEarly=30/45/70` with walls of
**911 ms / 1.365 s / 2.124 s**. **Those three arms held no stream at all**, so
the earlier phrasing here — "stayed at 93 through three further arms" — reads as
three further *loaded* arms holding the count up, and that is not what happened.
The plateau is real and is the opposite reading: **the count did not fall back
after the load stopped**, across three minutes in which nothing was holding a
stream. The part of the voided capture relied on is the S=10/S=20 ladder and the
post-load `db_backends` series; the S=30/45/70 arms are relied on for **nothing**
except establishing that they held nothing. That is the non-prompt release
`FINDINGS.md:2527` describes, seen as a plateau rather than as a code read.

---

## Part B — rate versus concurrency. **Concurrency at a latency-bearing endpoint is by far the cheaper route; the clean single-variable isolation was NOT run**

This is the scenario's chartered contribution, and the conclusion below is
**weaker than the one first published here**. What it said was "concurrency
drives it, and it is not close". A review of the matrix against its own metric
showed that the decisive comparison changes two variables at once and that the
argument silently switches metrics half-way through. The evidence is unchanged;
the reading of it is corrected, and the arms that would settle it are named as
outstanding.

Five arms, one replica. **Correcting this section's own sentence:** they were
**not** "each from a recreated and settled stack". The reset between arms is
`docker compose restart controller1 controller2 controller3` plus a 70 s settle
(`B-arm.sh`), which is a restart and not a recreate; and **B3 got no reset at
all**. `B0-control.txt` is the one genuine recreate: a stack brought up from
`down -v` settles from 59 to **68** client backends over ~7 minutes and stays
there, `/readyz` 200 throughout, no sample near the budget.

| Arm | endpoint | workers | requests / span | **avg rate** | **meanInFlight** | **maxInFlight** | TCP→5432 floor → after | in-window `db_backends` | saturated? | 401 |
|---|---|---:|---:|---:|---:|---:|---|---|---|---:|
| **B2** | fast GET | 8 | 1200 / 60.0 s | **20 /s** | **0.03** | 8 | 68 → 77 | 72 → **78**, all 17 instants read | **NO** | **0** of 1200 |
| **B1** | fast GET | 8 | 19623 / 8.005 s | **2451 /s** | **7.99** | 8 | 67 → 100 | `unavailable` × 5 | **YES** | 6242 (31.8 %) |
| **B3** | fast GET | 200 | 1200 / 60.0 s | **20 /s** | **0.69** | 200 | **80** → 100 | `unavailable` × 19 | **YES** | 96 |
| **B4** | fast GET | 200 | 15912 / 8.117 s | **1960 /s** | **198.35** | 200 | 65 → 100 | `unavailable` × 5 | **YES** | 4803 |
| **E-60** | claim long-poll | 60 | 180 / 60.3 s | **3 /s** | **59.89** | 60 | 67 → 99 | **100** on 13 of 14, 1 `unavailable` | **YES** | **0** — every request 200 |

**Units in that table, which the previous version's single "client backends"
column mixed.** The floor→after column is `TOTAL_ESTABLISHED_TO_5432` read from
`/proc/net/tcp` inside the Postgres container — **it counts the two agents' and
`psql`'s connections too, so it is not a controller figure**. The `db_backends`
column is `pg_stat_activity` filtered to `datname IS NOT NULL`, which is the
right quantity against the 97-slot budget, and for B1/B3/B4 **the entire
in-window series is `unavailable`** — Postgres refused the sampler, which is the
strongest saturation reading available but is not a number. The two columns are
never added or compared. `B-B{1,2,3,4}.txt`, `E-60.txt`, per-second histograms in
`B-histograms.txt`, commands in `B-arm.sh`.

**B3 is confounded by carry-over and it is the arm nearest the budget.** B2 ended
at 77 established; nine seconds later B3 started from a floor of **80**, against
B1's 67, B2's 68 and B4's 65. B3 therefore ran on B2's leftover stack, **17
connections from the 97-slot budget**, and reached 100. Its saturation cannot be
separated from its floor, and every use of B3 below carries that.

**What the matrix actually supports.**

- **What saturates Postgres is the number of api-pool connections checked out at
  the same instant**, and rate and concurrency are two routes to it. That much is
  unchanged and is what the five arms jointly show.
- **The concurrency route is far cheaper — but the comparison that shows it
  changes TWO variables.** E-60 held 60 requests genuinely in flight at a
  measured **2.98 /s** and pinned Postgres for the whole window; B2 held 8
  workers at 20 /s and did not saturate. **E-60 changes concurrency *and*
  endpoint**: `POST …/claim?timeout=20s` is a 20-second long-poll, against B2's
  `GET /api/v1/runs?jobName=edge-tick` at ~2 ms. So the decisive comparison is
  **cross-endpoint**, and it is stated here as a confound rather than only as
  E-60 "carrying the conclusion".
- **The within-endpoint isolation is B3 — the arm this runbook itself
  discounts.** B2 and B3 share the fast GET and the same 20 /s average and differ
  only in worker count; B3 saturates. But B3's 20 /s is an *average*: `-delay`
  releases 200 workers together, so the histogram shows **200 requests inside one
  second every ten seconds**. B2-vs-B3 is therefore about **burst width**, not
  worker count as such — and, per the paragraph above, it is also the arm with a
  contaminated floor. **The clean single-variable comparison was not run.**
- **And on the table's own concurrency metric the matrix reads against the
  thesis.** The column that was published was `meanInFlight`, and **B3 saturates
  at meanInFlight = 0.69 — lower than B1's 7.99, and B1 needed 2451 /s to get
  there.** A threshold that sits between B2's 0.03 and B3's 0.69 cannot be the
  causal variable: 0.69 mean in flight cannot hold 100 connections. The argument
  as first written **switched silently to `maxInFlight`** (8 vs 200) to make the
  ordering work. Both columns are now printed so the reader can see it.
- **Why the fast-GET arms cannot settle it, and this is structural rather than a
  tooling failure.** In-flight = rate × latency, so at a ~2 ms endpoint high
  concurrency is unreachable without high rate: rate and mean concurrency move
  together by construction. **High concurrency at low rate requires a
  latency-bearing endpoint**, which is precisely why the concurrency corner had
  to change endpoint. The confound is designed in.

**Therefore, stated at the strength the evidence supports: concurrency at a
latency-bearing endpoint is by far the cheaper route to `max_connections` — 60
long-polls at ~3 /s pin the server where 8 workers at 20 /s do not — but the
single-variable isolation of concurrency was not run, and no ordering of the
five arms by either in-flight metric supports a stronger claim.**

**The outstanding measurement that would settle it**, recorded rather than
implied: a **dose-response on one endpoint** — E-20, E-40, E-60 against the same
claim long-poll, varying only concurrency, from the same recreated floor. Three
points on one curve at one endpoint would separate concurrency from endpoint and
give the threshold a shape. It was not run: another scenario held the rig when
this correction was made.

- **The rate route is real and independent.** **B1** reproduced `:2517` almost
  exactly from a clean floor — 8 workers, `maxInFlight=8`, **2451 req/s**,
  **31.8 % 401** against `:2517`'s ~32 % — with only 8 in-flight handlers, which
  cannot themselves hold more than ~8 api-pool connections. So something that
  **outlives the request** must be filling the pool, and the only candidate in
  the code is still the per-request undeadlined
  `go func(){ TouchPAT(context.Background()) }()` at `auth.go:79`. **Not proven
  here either** — no capture reads that goroutine — but the B1/B2 pair rules out
  the alternative that 8 concurrent handlers are enough on their own.
- **`FINDINGS.md:2535`'s own caution is CONFIRMED, not overturned.** It says *"Do
  not read this entry as '8 concurrent requests at any rate will do this.'"*
  **B2 is that experiment and it does not saturate**: 8 workers, 20 req/s, 60 s,
  **1200 of 1200 requests 200, zero 401s**, +9 established. The entry was right to
  call its trigger a rate.

**The rate ratio, shown as a division and with both endpoints named** — because
this runbook said "850 times less" while `README.md` said "~800×" for what reads
as the same claim, and neither was labelled derived. **One figure is used from
here on, and it is the within-session one:** B1's **2451 /s** (19623 ÷ 8.005 s,
`GET /api/v1/runs?jobName=edge-tick`) ÷ E-60's **2.98 /s** (180 ÷ 60.348 s,
`POST /api/v1/agents/{id}/claim?timeout=20s`) = **~820×**, derived, and
**cross-endpoint**. The separate comparison against `FINDINGS.md:2517`'s
2554 /s trigger gives ~856× and is *cross-session as well as cross-endpoint*;
it is mentioned once and not carried.

**And the thing `-delay` cannot do, which is why the runbook's Amendment 1
mattered.** `:2535` proposed settling this with "`-c 8` with a per-worker delay,
so 8 in flight at ~50 req/s". Measured: `-c 8 -delay 400ms` gives
`meanInFlight=0.03`, not 8 — in-flight is rate × latency and the endpoint answers
in ~2 ms. **Holding N in flight at a low rate requires a latency-bearing
endpoint**, which is why the concurrency corner had to be the claim long-poll
and the SSE stream rather than a paced GET. Anyone re-running this should not
expect `-delay` to hold concurrency.

**Cross-arm caveat.** The IP→service mapping is re-assigned by each `down -v` /
`up` (`B-B4-maxrate-200w-pg.txt` resolves `172.20.0.7` to controller3 where an
earlier capture resolved it to controller2), so **only totals are comparable
across recreates**; per-replica splits are comparable only within one stack
instance.

---

## Part C — what breaks first: **nothing that an operator can see**

### C1 — the health surface. In-window, at last, and it does not move

`FINDINGS.md:2525` could only say `/readyz` was 200 at one point ~3 minutes
*after* a load window. Every arm above carries `/readyz` and `/healthz` read
**inside the same loop iteration** as the backend count. Across the saturated
arms:

**The unit, first, because the published headline got it wrong.** One row of the
health series is **one port at one sample instant**, so a 14-sample window across
three replicas yields 42 rows. `w6-pgsample.sh` called those rows "samples", and
this runbook inherited the word — which triple-counts the denominator. Both
denominators are given below, and the tool now prints both.

| Arm | sample instants | replica-readings (instants × 3) | saturated readings | `/readyz` still 200 |
|---|---:|---:|---:|---:|
| E-60 | 14 | 42 | 42 of 42 | **42** |
| B4 | 5 | 15 | 15 of 15 | **15** |
| B3 | 19 | 57 | 57 of 57 | **57** |
| B1 | 5 | 15 | 15 of 15 | **14** (one 503, on the loaded replica) |
| post-A3 standalone | 5 | 15 | 15 of 15 | **15** |
| **total** | **48** | **144** | **144** | **143** |

**143 of 144 saturated in-window replica-readings read 200 — i.e. 47 of the 48
sampling instants read 200 on all three replicas.** *(An earlier version of this
line said `144 of 145`. The per-arm figures were and are correct; the total was
wrong on both sides, and it was the headline of the wave's one violation. It is
corrected here, at `FINDINGS.md`, in `README.md` and in the tool. **The commit
subject and body of `1c9988f` still carry `144 of 145` and cannot be changed** —
a reader who finds that number in the git log should read this table instead.)*

The single 503 is on port 18081 during B1 — the replica taking 2451 req/s — and
is **inferred** to be the api-pool `Acquire` losing its 3 s race, exactly as
Amendment 2 predicted. **The inference is labelled because the body was not
captured**: `internal/controller/server.go:318-332` returns 503 for *both*
`shuttingDown` and a failed `Ping`, and the capture records only the status code.
Nothing was draining at 08:32, so the `Acquire` reading is very likely right —
but it is a code-read plus an absence, not a measurement. On that reading it is a
symptom of *load on that replica*, not of Postgres exhaustion, and it did not
recur in B4 which had 25× the concurrency on the same replica.

### C2 — **no ERROR line was observed**, and that is a weaker statement than the one first published here

Predicted: the background pool (32) starves before the api pool (128).

**What is claimed: across the window `C-bgstarve.txt` covers, the three
controllers emitted zero `"level":"ERROR"` lines.** **What was claimed before
review, and is withdrawn: "background jobs did not starve."** The capture cannot
carry that, for a reason that is arithmetic rather than presentational, and the
full list of what it lacks is archived beside it as
`C-bgstarve-LIMITS.txt`. The four points, briefly:

- **The two numbers are from different windows.** The file reads `total lines
  last 4m: 604` and `ERROR lines last 12m: 0`. This runbook and `FINDINGS.md`
  both said "the same command"; it is `--since 4m` against `--since 12m`, so the
  604-line liveness control does **not** bound the window the zero was taken
  over.
- **The window endpoints are not in the capture.** No capture timestamp, no
  command text, no service scope and no log-level configuration were recorded, so
  neither `--since` can be shown to cover any particular arm. A 4-minute window
  ending at capture time covers E-60 (08:37) but not B1 (08:32).
- **The error-class section is empty with no positive control.**
- **And the substantive one: zero errors is consistent with zero attempts.**
  B1's window is **8 s** (08:32:06.009 .. 08:32:14.015). The log archiver ticks
  at **60 s** and the stuck-run reaper at 30 s — both read off `D3-after.txt`,
  where controller1 boots at 07:55:52 and logs its first `log archiver error` at
  07:56:52, with reaper errors 30 s apart. **An 8-second window is shorter than
  the archiver's period, so B1 can contain zero archiver executions, and a job
  that never ran cannot log an error.** E-60's 60 s spans at most one tick.

**Outstanding measurement, and it needs the rig:** an in-window count of
background-job *executions*, successful ones included, so the zero can be read
against a non-zero denominator. It was not taken — another scenario held the rig
when this correction was made — and until it is, this limb says only that
nothing was logged.

**What does survive, and it is the part the section's conclusion rests on.** The
only starvation seen anywhere in this scenario is in Part D, on a controller that
had **just restarted** and therefore had to open new connections: `log archiver
error`, `stuck-run reaper list error`, `queued-run reaper lock`, `appsource
reconciler` — all naming `too many clients already`, all with timestamps and a
command in `D3-after.txt`. That is a **positive** observation on a capture that
bounds itself, and it does not depend on `C-bgstarve.txt` at all.

**So the rule is: a warm pool hides the exhaustion from anything that does not
need a new connection.** A pool that already holds its connections keeps serving
from them; only a component that needs a *new* connection ever sees the fault —
which is exactly what Part D's restarted controller demonstrates positively.
Combined with C1 that means a saturated cluster produces **200 on `/readyz`, 200
on `/healthz`, and no ERROR line in what was sampled of the controller log**.

### C3 — the open SSE question, SETTLED

`FINDINGS.md:2537` handed this scenario, verbatim, *"The '200 with backfill and
then a silently dead stream' question is therefore still OPEN"*. It is now
answered, and the uncaptured guess it was left with is **half right**.

From `A3.txt` / `A3-sse40.csv`, the 16 streams past the cap:

```
#   status connect_ms  firstEvent_ms  events  alive   note
25  200    0.5         0.5            14      false   server closed the stream
…
39  200    1.8         1.8            18      false   server closed the stream
```

- **The 200 is real, and so is the backfill.** Every one of the 16 got `200` and
  **14-18 log events** — a complete, correct view of the run as it stood.
- **The stream is not silently *dead*; it is silently *closed*.** The server ends
  the response body. A client sees EOF, not a hang. That distinction matters for
  a re-runner: the failure is indistinguishable from a normal end-of-stream, and
  an EventSource will simply reconnect — into the same refusal.
- **Nothing anywhere says why.** Status 200, no `error` event, no `truncated`
  event, and — per C2 — **no controller log line**. The client's only evidence
  that it is not watching a live run is that no further events arrive.

Mechanism, and it is exactly the code read: the headers and the whole backfill
are written at `sse.go:55-103`, *before* `:118` reaches
`_ = s.store.ListenForNotify(...)`, whose first act is
`p.listenPool.Acquire(ctx)` (`postgres.go:1665-1667`). The acquire fails, the
error is discarded by the `_ =`, `handleRunEvents` returns, and the response
ends. **The `_ =` is the whole finding**: one assignment converts "the database
is full" into "your log ended".

`FINDINGS.md:2536` asked for this to be checked against the archive rather than
recalled; the archived numbers are now `aliveAtEnd=24 diedEarly=16 non200=0` in
`A3.txt` and the per-stream table in `A3-sse40.csv`.

### C4 — the 401 mechanism, corroborated rather than re-filed

`:2530` labels the 401 explanation **INFERRED**, because no capture reads the
failing `err`. This scenario cannot read it either, but it gets much closer:
inside a single second of B1, the **same client with the same valid PAT** gets
both

```
08:32:06.182 seq=94  code=500 body="failed to connect to `user=unified database=unified`: … FATAL: sorry, too many clients already (SQLSTATE 53300)"
08:32:06.189 seq=122 code=401 body="unauthorized"
```

(`B-B1-maxrate-8w-errbodies.txt`; B3 and B4 reproduce it.) The 500 is the same
condition surfacing through a handler that propagates its error; the 401 is the
same condition surfacing through `ServerAuth`, which cannot tell a DB error from
an unknown token (`auth.go:77-79`). **Still not the failing `err` itself, so the
label stays "inferred" — but the two responses are now co-captured in the same
second from the same token, which is as close as a black-box capture gets.**

---

## Part D — recovery: the prediction was WRONG, and the reason is the finding

**Predicted:** a controller restarted while Postgres is at `max_connections`
fails `store init`, exits(1), and — with no `restart:` policy — stays down.

**Measured:** it comes straight back. `D2-restart.txt` — restart issued
07:55:50.323 with Postgres refusing every connection; the container was `Up` and
`/readyz` 200 by 07:55:53, and `RestartCount=0`, `ExitCode=0` (`D3-after.txt`).
**The mechanism is that stopping the process is what frees the connections it
then needs**: a controller holding ~300 pooled connections releases all of them
on SIGTERM, and its replacement opens into the space it just vacated.
`FINDINGS.md:43`'s crash-loop needs Postgres to be unreachable for a reason that
is *not* this controller's own pool — a `pgbouncer` boot race, say. **Cite `:43`;
this is not a re-file, it is a scope note on it**, and it means the remedy
`:2534` calls "the obvious operator response with a documented failure mode" is
in fact safe in *this particular* state.

**I5's re-election bound is met at 89 ms; the other bounds were not exercised.**
The spec's text (`:52`) names three things — leader re-election ≤ seconds, the
stuck-run reap ≤ `staleAfter` 90 s + interval 30 s, **and** "the bounds in
`docs/high-availability.md`". Only the first was measured: `controller1` logged
`received shutdown signal, draining...` at **07:55:50.644**, and `controller2`
logged `scheduler became leader` at **07:55:50.733** — **89 ms**, under load,
with Postgres refusing new connections. **"I5 is MET" was asserted on that one
bound and is corrected here**: the stuck-run reap was never timed (Part D's only
reaper evidence is *error* lines from a restarted controller, which is the
opposite of a reap completing), and `docs/high-availability.md`'s bounds were
not enumerated, so the class behind "MET" is unverified. **No violation is
claimed and none is available** — an unexercised bound is not a missed one.

**The finding is what the restart did NOT fix.** With the connections read from
`/proc/net/tcp` inside the Postgres container — the only way to read them at all
while `psql` is refused (`D4-conn-owners.txt`, 07:59:11):

```
   61  controller3
   22  controller2
   17  controller1   (just restarted)
   TOTAL_ESTABLISHED_TO_5432: 100
```

**The saturation is owned by ONE replica — the one that served the SSE fan-out —
and restarting a different replica moved the total not at all.** controller1
released ~300 pooled slots and the total was still pinned at 100 at the next
reading. **The "why" is INFERRED and nothing measures it:** the restart was at
07:55:50 (`D2-restart.txt`) and the next connection reading is at 07:59:11
(`D4-conn-owners.txt`) — **a 3 min 21 s gap with no connection sample in it**,
because `D2-restart.txt` polls `/readyz` and container state only. "The survivors
expanded into the freed slots faster than the sampling interval" is therefore a
mechanism offered to explain two endpoints, not something observed happening;
what is *measured* is that the total was 100 before and 100 after. **The
attribution itself is solid and is not affected:** 61/22/17 = 100 read from
`/proc/net/tcp` with an explicit IP→service map, corroborated by `D5-recovery.txt`
where restarting the owning replica drops the total to 43. A rolling restart, or
an operator restarting "the controller" they happen to be looking at, has a
**one-in-three** chance of touching the replica that matters. Restarting the
right one cleared it immediately: 100 → **43 established within 11 s**, `psql`
answering again at 58 backends (`D5-recovery.txt`).

**How long it lasted, stated at the strength of the capture.** Postgres first
refused at **07:49:50** and was still refusing at **07:59:29** when the owning
replica was restarted — **≥ 9 min 39 s of continuous saturation with no
self-recovery**, ended by operator action. **No upper bound is established: that
window was ended deliberately.** pgxpool's `MaxConnIdleTime` of 30 m is a code
read, not a measurement, and nothing here says the cluster would have recovered
at 30 m or ever. **Do not read "never" into this.**

---

## Part E — the claim long-poll, and it is the cheapest trigger in the campaign

`ClaimNextRun` is already 49 % of the idle floor from **two** agents. The
question was what N concurrent long-polls cost.

`E-60.txt`: one enrolled synthetic agent identity, **60 concurrent
`POST /api/v1/agents/{id}/claim?timeout=20s`** against one replica for 60 s.

```
requests=180 wall=1m0.348s  maxInFlight=60  meanInFlight=59.89
status   200=180 errors=0
client backends: 67 -> 100, held at 100 for the whole window
/readyz 200 in 42 of 42 saturated replica-readings (14 instants x 3 replicas)
```

**180 requests. Three per second. Every one of them returned 200. Postgres
pinned at `max_connections` for the entire minute.**

The mechanism is in the handler and is not a bug in itself:
`handleAgentClaim` (`internal/controller/api_agent.go:127-155`) is a **1 Hz
polling loop inside the request** — `claimPollInterval = 1 * time.Second`,
`maxClaimTimeout = 60 * time.Second` — plus one `UpsertAgentOnClaim` per request
at `:150`. So an in-flight long-poll is not one connection held; it is one
connection **re-acquired every second**, and 60 of them keep the api pool
permanently busy. The pool grows to serve them and, per `:2527`, does not shrink
promptly.

**Why this is the scenario's most operationally important number.** Sixty agents
long-polling for work is not load, not abuse and not a misconfiguration — it is
sixty agents doing the one thing agents exist to do, at a rate of three requests
per second across the whole fleet. On this rig that is enough to pin the
database, and **no surface reports it**: the agents all get 200, `/readyz` is
200, and the controllers log nothing.

**What this does NOT license, and the charter's whole point.** It does **not**
mean "unified-cd supports 59 agents". `test/ha` runs stock
`max_connections=100`; the repository's own `docker-compose.yaml:30` starts
Postgres with **`max_connections=1000`** and `docs/operations.md:173` tells
operators so, and on that configuration 3 × 304 = 912 fits. The transferable
statement is the **shape**: *concurrent long-polls consume api-pool connections,
the pool does not give them back promptly, and the ceiling that is hit is the
database's, not the pool's — so the fleet size a deployment tolerates is set by
`max_connections` against `replicas × 304`, and nothing in the product warns when
that budget is spent.*

**"In proportion to their number" is withdrawn: it was a proportionality claim
from one point, and that point is not proportional.** Measured on this arm, on
one instrument on both sides: established TCP to 5432 went **67 → 99**, i.e.
**60 concurrent long-polls added ~32 connections, not ~60**. **The scaling law is
not measured**, and it cannot be read off this arm in either direction, because
the observation is **right-censored**: the in-window `db_backends` series sits at
**100**, the server's own ceiling, on 13 of its 14 instants. Demand above the
ceiling cannot appear in the reading. So "+32" is a floor on what 60 long-polls
wanted, not a coefficient. The dose-response arms named in Part B (E-20, E-40,
E-60 from a common floor) are what would give it a shape.
**One laptop cannot produce the number, only the relationship. The number an
operator needs is arithmetic on their own `max_connections`, and it should be
computed, not extrapolated from here.**

---

## Findings filed

1. **`docs/high-availability.md:289` and `docs/operations.md:154` promise a
   readiness surface that rotates a DB-broken replica out; measured in-window,
   `/readyz` reads 200 in 143 of 144 saturated replica-readings (47 of 48
   sample instants) across four independent
   arms while Postgres refuses every new connection — and **no controller `ERROR`
   line was observed** (see C2 for what that capture does and does not
   support).** This is the settled version of the question `FINDINGS.md:2532`
   deliberately left open and handed here.
2. **Observation: sixty concurrent agent claim long-polls — three requests per
   second, every one answered 200 — pin Postgres at `max_connections`**, because
   the claim handler polls the database once a second inside the request; the
   API surface and the health surface are clean throughout and no `ERROR` line
   was observed. **Filed as a capacity relationship, not as a concurrency
   conclusion** — the arm changes endpoint as well as concurrency and the
   single-variable isolation was not run (Part B).
3. **Observation: an SSE subscriber past the connection ceiling receives 200 and
   a complete backfill and then has its stream closed by the server, with no
   error event, no status event and no server-side log line** — settles
   `FINDINGS.md:2537`.
4. **Observation: connection exhaustion is owned by a single replica, and
   restarting any other replica returns its connections to the pool of the ones
   still holding them**, so the natural operator remedy — and a rolling restart —
   fixes nothing unless it happens to hit the right replica.
5. **Campaign asset — observation: `w6-pgsample.sh` could not survive the
   saturation it was built to measure**, and its own summary compared
   background-worker-inflated totals against the wrong budget; plus
   `w6-synth-agent.sh heartbeat` failed the runs it was called to protect.

Not filed, deliberately: the 24-stream ceiling and the ~2450 req/s rate trigger
(both are `FINDINGS.md:2517` measured properly — **cited, not re-filed**); I5
(its re-election bound met at 89 ms, its other bounds not exercised); the
crash-loop at `FINDINGS.md:43` (**cited**, and given a scope note rather than a
new entry).

**Outstanding measurements, all needing the rig and none taken** (another
scenario held it when these corrections were made): (a) the **E-20 / E-40 / E-60
dose-response on one endpoint** that would isolate concurrency; (b) an
**in-window count of background-job executions** to give C2's zero a
denominator; (c) a **B3 re-run from a reset floor** to remove its carry-over
confound; (d) a **connection sample inside Part D's 3 min 21 s gap** to measure
the survivors expanding rather than infer it.
