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

*(filled in below after execution)*
