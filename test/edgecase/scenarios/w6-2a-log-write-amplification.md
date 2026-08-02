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
that **8 concurrent requests pin the server at `max_connections`**, with
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

*(Filled in after execution. The plan above is committed first, per campaign
practice, so the plan-versus-outcome delta is visible in the history.)*
