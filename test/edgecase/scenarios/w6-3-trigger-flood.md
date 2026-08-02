# W6-3 — the trigger flood, and what the absence of a rate limiter actually costs

**Charter: demonstrate that nothing on the run-creation path is rate limited,
price that absence, and put I1 under the flood.** This is W6's last scenario.

**Part A is deliberately NOT a throughput contest.** W6-1 settled that
concurrency — not rate — drives saturation on this rig, and `test/ha/nginx.conf`
has no upstream `keepalive` with `worker_connections` at 512, so a rate contest
through the LB measures TIME_WAIT exhaustion in the rig rather than any product
limit. Demonstrating an *absence* needs only enough load to make the point plus
an honest statement of the price. The load below is sized to that and no larger,
and every rate-bearing arm addresses a **named controller** on the
`ctrlports.override.yaml` ports — except **Part C2, where the LB is the object
under test and is targeted on purpose**, which is stated at the arm.

**Invariant attacked: I1**, quoted verbatim from
`docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:48`:

> | I1 | **Run accounting** — every API-accepted run reaches exactly one terminal state; no phantom runs from duplicate fires/webhooks |

The table spans `:48-54`; I4 is `:51` and I5 is `:52`. Line numbers re-checked
against the file for this runbook, because three W4 runbooks got them wrong.

---

## What this scenario inherits and must NOT re-derive or re-file

- **`FINDINGS.md:2517`** (W6-infra, Task 1). A sustained ~2,550 req/s held at 8
  in flight pins Postgres at `max_connections`, `/readyz` still reads 200, and a
  valid admin PAT 401s on about a third of requests. **Cite; do not re-file.**
  Its 9x over-commit arithmetic is **scoped to this rig**: `test/ha` runs stock
  `max_connections=100`, while the repository's own `docker-compose.yaml:30`
  starts Postgres with `max_connections=1000` and `docs/operations.md:173` tells
  operators so (3 x 304 = 912 < 1000). Every claim below about a real deployment
  says which of the two it means.
- **`FINDINGS.md:1398`** (W2-9). The scheduler examines only the **50 oldest**
  `Pending` runs — one hardcoded `limit = 50` at
  `internal/controller/scheduler.go:58`. **`:1399` forbids double-counting it**:
  "`scheduler.go:58` is one defect and must not be double-counted." Part B
  re-triggers it on contact and **cites it**; Part B files nothing about the
  ceiling itself. What Part B contributes is the number `:1398` does not have —
  the operator-visible end-to-end latency of an *unrelated* run behind a flood.
- **`FINDINGS.md:495`** proposes a de-credentialled agent as a permanent,
  un-backed-off ~8.3 req/s rejection source. Part D uses it, and **AMENDMENT 5
  below predicts the 8.3 figure does not transfer.**
- **`FINDINGS.md:515` / `:563`.** Ten of eleven "leader-elected" background jobs
  are per-tick mutexes running 2.15-2.40x per nominal interval on three
  replicas. A load multiplier on every measurement here.
- **W6-1's settled result.** Concurrency drives saturation decisively: 8 workers
  at 20 req/s did not saturate; 60 concurrent claim long-polls at 3 req/s did.
  Rate is a genuine but ~800x more expensive second route. **`/readyz` does not
  degrade** (200 in 144 of 145 saturated in-window samples) — this scenario does
  not predict otherwise. **Background jobs do not starve first** (zero
  controller `ERROR` lines across two saturated arms).
- **The idle floor** (`README.md` §"The idle floor"): ~71.8-72.0 q/s across three
  controllers and two agents with zero runs, `ClaimNextRun` ~49% of it, 73-74 of
  100 backends at rest. Every q/s number below is read net of this.

---

## Corrections to inherited facts, established BEFORE execution

Six consecutive waves have had a plan's "verified code facts" corrected by
execution, and the pattern is that `file:line` claims hold while **mechanism**
claims fail. Both classes were re-checked before a single arm ran, and this time
**two mechanism claims in the brief are wrong** — one of them removes Part E's
expected finding entirely.

### The `file:line` claims — every one holds

| Claim | Verified |
|---|---|
| No limiter middleware, `server.go:285-293` | `routes()` opens at `:284`; the `s.r.Use(...)` stack at `:285-293` is exactly `middleware.Recoverer`, `middleware.RealIP`, `accessLogMiddleware`, `s.metricsMiddleware`, `securityHeadersMiddleware`, `s.originCheckMiddleware`. Six middlewares, none of them a limiter. HOLDS |
| `enrollment_limiter.go:14` | `const enrollmentLimiterCapacity = 4096`, verbatim. HOLDS |
| `scheduler.go:58` = the 50-row ceiling | `n, err := st.TransitionPendingToQueued(ctx, 50)`, verbatim. HOLDS |
| `cmd/controller/main.go:203-205` = the three retention flags | `:203` `--audit-retention-days`, `:204` `--run-retention-days`, `:205` `--log-trim-days`. HOLDS as *locations*; see AMENDMENT 1 for the *values* |
| `FINDINGS.md:2517`, `:1398`, `:1399`, `:495`, `:515`, `:563` | all six resolve to the entries the brief names. HOLD |
| `docs/superpowers/...:48` is I1 | quoted above, verbatim, from the file. HOLDS |

### AMENDMENT 1 — the brief's Part E premise is FALSE for one of its three variables: audit retention defaults to 90 days, not 0

The brief states that `UNIFIED_AUDIT_RETENTION_DAYS`, `UNIFIED_RUN_RETENTION_DAYS`
and `UNIFIED_LOG_TRIM_DAYS` "all default to **0 = keep forever**". Two of three
do. **`auditRetentionDaysDefault()` returns 90** —
`cmd/controller/main.go:29-34`, `const defaultDays = 90` at `:30`, returned at
`:33` when the env var is unset. Only `runRetentionDaysDefault()`
(`:47-51`) and `logTrimDaysDefault()` (`:64-68`) return `0`.

This matters because it changes what Part E can find. The violation limb the
brief describes — "a documented promise that contradicts it" — needs a doc that
states a *bounded* default where the code keeps forever. Part E's survey is run
untruncated below and reports hit counts; its result is stated as a result
whether or not it produces a finding.

### AMENDMENT 2 — "the only rate limiter in the product is the enrollment limiter" is right about *admission* and incomplete about the *class*

The enumeration was verified rather than inherited, because the campaign rule is
that a claim of full enumeration must be checked.

`grep -rn "rate.NewLimiter\|golang.org/x/time/rate" internal/ cmd/ --include=*.go`
(excluding `_test`) returns **3 hits, all in `internal/controller/enrollment_limiter.go`**
(`:11`, `:25`, `:44`). So there is exactly **one token-bucket admission limiter**
in the product, and the brief is right about that one.

But `internal/controller/` holds **two** files whose type name ends in
`Limiter`, and the second is not a token bucket and never returns 429:

| | `enrollment_limiter.go` | `credential_touch_limiter.go` |
|---|---|---|
| Kind | admission (rejects the request) | suppression (skips a DB write) |
| Rate | `rate.NewLimiter(rate.Every(6*time.Second), 5)` (`:44`) | one touch per id per **5 minutes** (`:35`) |
| Key | provider + normalized remote IP + policy (`:36`) | credential id (`:30`) |
| Client-visible | **yes** — 429 + `Retry-After: 6` (`api_agent_enrollment.go:452-456`) | **no** |
| Reachable from | exactly 3 sites: `api_agent_enrollment.go:271` (one-time-token exchange), `:313` (kubernetes exchange), `:408` (refresh) | 1 site: `agent_auth.go:88` |

**Both are on agent-credential paths. Neither is on the run-creation path, and
nothing else on any path is.** That is the enumerated form of Part A's claim.

### AMENDMENT 3 — the price asymmetry: the *authenticated human* path is the more expensive one, per request

`auth.go:77-79` fires an **undeadlined, unthrottled** goroutine per
PAT-authenticated request:

```go
go func() { _ = st.TouchPAT(context.Background(), pat.ID) }()
```

The agent-credential path's equivalent is throttled to once per credential per
five minutes (`agent_auth.go:88` guarded by `credentialTouches.shouldTouch`).
So a PAT flood costs one extra background `UPDATE` per request that an agent
flood does not, and that write is fire-and-forget with `context.Background()` —
it survives the request's own cancellation.

Code-read cost of one accepted `POST /api/v1/runs`: `GetPATByHash`
(`auth.go:77`) -> `GetJob` (`api_runs.go:49`) -> `CreateRun` (`api_runs.go:83`)
-> `InsertAuditLog` (`audit.go:227`, synchronous, in `auditLogMiddleware`), plus
the async `TouchPAT`. **Predicted ~4 synchronous + 1 asynchronous statement per
accepted run**, and Part A measures the realised multiplier against the 71.8 q/s
idle floor rather than asserting it.

### AMENDMENT 4 — I1's three limbs are not equally reachable, and the code says which. This is the design of Part C

I1's own text has three clauses. Reading them against the code before measuring
gives three different predictions, and a spot check would conflate them.

1. **"reaches exactly one terminal state."** Protected by a real CAS.
   `Postgres.FinishRun` (`internal/store/postgres.go:746`, with
   `MarkRunFinished` at `:736-739` delegating to it) runs
   `UPDATE runs SET status = $1, updated_at = NOW() WHERE id = $2 AND status NOT IN ('Succeeded', 'Failed', 'Cancelled')`
   (`postgres.go:759-762`) and reports the CAS miss through
   `tag.RowsAffected() > 0` (`:766`). Nine call sites can write a terminal
   status (`api_runs.go:374`, `:406`; `stuckrun_reaper.go:84`;
   `queuedrun_reaper.go:73`; `scheduler.go:330,344,358,374`; `postgres.go:537`,
   `:640`; `api_agent.go:608`, `:824`) and all of them route through it.
   **Predict: HOLDS.** The measurement that makes this more than a code-read is
   that `unifiedcd_runs_finished_total` increments **only on a successful CAS**
   (`internal/metrics/store.go:38-44`, and the comment at `:46-48` says so), so
   *summed counter delta across the three replicas* versus *distinct runs in a
   terminal state* is a direct double-write detector.
2. **"no phantom runs from duplicate fires/webhooks."** There is **no
   idempotency mechanism anywhere on run creation**: `CreateRun` is an
   unconditional `INSERT` with no `ON CONFLICT` (`postgres.go:245-254`), `runs`
   carries no `UNIQUE` index other than `runs_pkey`
   (`migrations/001_init.up.sql:460-463`; the other run indexes at
   `008_run_indexes.up.sql:10-12`, `011:11-12`, `017:5` are all non-unique), and
   no handler reads any idempotency header. N identical client POSTs therefore
   create N runs **by design** — that is not a phantom. The reachable phantom
   mechanism is not the client's duplicate but the **load balancer's**, and the
   product *documents* one: `docs/high-availability.md:260-261` tells operators
   to set `proxy_next_upstream error timeout http_502 http_503 http_504` with
   `proxy_next_upstream_tries 3`, and `:254` points at `test/ha/nginx.conf` as
   "a complete working example" (it carries exactly those directives at
   `nginx.conf:24-26`). **Predict: no phantom** — nginx has not passed
   non-idempotent methods to the next upstream by default since 1.9.13 — **but
   that is a prediction about nginx's default, not a product fact, so Part C2
   measures it instead of citing it.** If it is refuted, the finding is an I1
   violation reachable through the product's own published LB configuration,
   which is a documented-contract limb as well as an invariant one.
3. **"reaches ... a terminal state" at all.** The reachable limb. A run stuck
   `Pending` behind `scheduler.go:58` has been API-accepted and reaches no
   terminal state. Part B produces it; Part C3 measures how long it persists and
   is careful not to write "never" for a window this session ended.

### AMENDMENT 5 — `FINDINGS.md:495`'s 8.3 req/s does not transfer to an idle de-credentialled agent, and the real floor is a different constant

`:495`'s ~8.3 req/s floor is the **`LogPusher`'s** re-flush rate with pending
batches outstanding — it belongs to an agent that is *executing a step*. An
agent that has been de-credentialled while idle has no pending log batches at
all. What it does have is the claim loop's error path
(`internal/agent/agent.go:416-422`): on any claim error it logs, sleeps a
**fixed `time.After(2 * time.Second)`**, and continues. There is no exponential
backoff and no give-up. **Predict: ~0.5 req/s per claim slot** (the rig runs
`MaxConcurrent` 1, `agent.go:218-221`), i.e. ~0.5 req/s per agent, plus whatever
the credential-refresh path adds — not 8.3. Note the contrast with
`internal/agent/retry.go:33-36`, which treats any status < 500 as permanent and
gives up: the claim loop does not use it, so a 401 there is retried forever.
Part D measures which of the two shapes an idle de-credentialled agent presents.

---

## The rig

`test/ha` (3 controllers behind nginx on :18080, Postgres at stock
`max_connections=100`, 2 Linux agents, Garage S3) plus two committed overlays:

```bash
cd test/ha
export COMPOSE_FILES="-f docker-compose.ha.yaml \
  -f ../edgecase/compose/logfault.override.yaml \
  -f ../edgecase/compose/ctrlports.override.yaml"
docker compose $COMPOSE_FILES up -d --build
test/edgecase/tools/w6/w6-build.sh
```

`ctrlports.override.yaml` exposes controller1/2/3 on 18081/18082/18083.
No new overlay is needed and **no product code, `manifests/`, `test/ha/` or
`workloads/podcap-job.payload.json` is touched** — this scenario injects no
fault at the network layer at all except the single `docker kill` in Part C2.

### Fixtures — all three already committed, none added

| Fixture | Role |
|---|---|
| `workloads/mutex-hog.payload.json` (`edge-mutex-hog`) | the flood target and the head-of-line blocker. `concurrency.mutex: edge-mutex`, `agentSelector: kind:linux`, `sleep 600`. The first run acquires the mutex at queue time (`postgres.go:547-555`) and holds it for ten minutes; every later run of the same job cannot leave `Pending`, which is exactly `scheduler.go:58`'s starvation shape |
| `workloads/unrelated-probe.payload.json` (`edge-unrelated-probe`) | the operator's unrelated run. `kind:linux`, one `echo`. Its trigger-to-terminal latency is Part B's number |
| `workloads/w6-probe.payload.json` (`edge-w6-probe`) | `kind:w6synth`, which no real agent carries. Part C2's runs use it so they Queue immediately and perturb neither the agents nor Part B's `Pending` backlog |

### Instruments

| Instrument | Used for |
|---|---|
| `tools/w6/bin/loadgen` | every flood arm. Named controller, `-mode sustained`, `-out` CSV, `-error-bodies` for any non-2xx sample. Its `maxInFlight` guard fires in both directions |
| `tools/w6/w6-pgsample.sh -p 18081,18082,18083` | backends per replica per derived pool **and** in-window `/readyz` / `/healthz` on the same grid |
| `tools/w6/w6-idleload.sh` + `w6-idleanalyze.py` | bounded Postgres statement-logging window; q/s in total, per replica and per statement class. Despite the name it is a generic window recorder |
| `/metrics` on each of 18081/18082/18083 | `unifiedcd_runs_created_total`, `unifiedcd_runs_finished_total`, `unifiedcd_agent_auth_events_total`, `unifiedcd_http_requests_total`. **Each replica owns its own registry (`internal/metrics/metrics.go:34`), so every number is a sum of three and every delta a difference of two sums** |
| `psql` in the `postgres` container | run-status census by `created_at` window; the ground truth the counters are reconciled against |

**The `nohup` hazard W6-1 hit is avoided by construction: every arm below runs
in the foreground.** No arm starts a background driver, so no arm can outlive its
tool call and contaminate the next capture. The one background process in the
whole scenario is Part D's fault, which is a *state change* on the controller
(revoked credentials), not a process, and it is reverted explicitly.

**A low-privilege PAT, not the admin one.** `requireMinRole("developer")` guards
the `dev` route group (`server.go:359`) that carries `POST /runs` (`:370`), so
**developer is the minimum role that can create a run**. Part A floods with a
freshly minted `developer` PAT to make the blast-radius claim honest.

---

## Parts, with predictions stated before the first capture

### Part A — the absence of a limiter, and its price

`loadgen -c 16 -n 1000 -mode sustained` at
`http://localhost:18081/api/v1/runs`, body `{"jobName":"edge-mutex-hog"}`,
`Authorization: Bearer <developer PAT>`. Concurrently: `w6-pgsample.sh -p` on a
2 s grid and a `w6-idleload.sh` window over the same interval.

**Sixteen in flight is chosen to be far under W6-1's demonstrated saturation
thresholds (24 SSE streams, 60 claim long-polls).** Part A is a limiter-absence
demonstration, not a saturation re-run.

**Predictions.** (a) **Zero 429 responses**, because no admission limiter exists
on this path (AMENDMENT 2). (b) 1000 accepted creations, no 5xx. (c) Query rate
well above the 71.8 q/s idle floor, with the per-accepted-run multiplier near
the ~4-synchronous-statement code-read of AMENDMENT 3. (d) **No Postgres
saturation and `/readyz` 200 throughout** — 16 in flight is a fifth of W6-1's
concurrency threshold, and predicting saturation here would contradict W6-1.
(e) 1000 permanent `runs` rows plus 1000 permanent `audit_logs` rows, from one
developer PAT, in under a minute.

### Part B — compounding with `scheduler.go:58`

Baseline first, on the quiet stack: three `edge-unrelated-probe` runs, trigger
to terminal, wall clock. Then, after Part A's backlog exists, one more.

**Prediction: the post-flood probe does not reach a terminal state for the
duration of the observation window**, because the 50 oldest `Pending` runs are
all mutex-blocked `edge-mutex-hog` rows and the probe is newer than all of them,
while **agent2 is idle and long-polling for exactly this work**. Baseline is
expected in the low single-digit seconds. **`FINDINGS.md:1398` is cited, not
re-filed.**

### Part C — I1 under the flood, in three limbs (AMENDMENT 4)

- **C1 — accounting reconciliation over Part A's window.** Four independent
  counts must agree: `loadgen`'s 2xx count; the summed
  `unifiedcd_runs_created_total` delta over three replicas; `SELECT count(*)`
  from `runs` in the window; and the count of distinct run ids. Predict: all
  four equal 1000. Any excess row is a phantom; any shortfall is a lost accept.
- **C2 — the phantom mechanism the product documents.** `loadgen -c 8 -n 400`
  against **the LB on :18080** (targeted deliberately — the documented
  `proxy_next_upstream` configuration is the object under test), job
  `edge-w6-probe`, with `docker kill -s KILL controller2` fired mid-window so
  nginx's retry conditions are genuinely met. Reconcile **rows created against
  requests sent**, not against 2xx: `rows > requests_sent` proves some single
  client request created more than one run and is unambiguous regardless of what
  status the client saw. Predict: `rows <= requests_sent`, i.e. no phantom.
  Refutation is a major I1 violation with a documented-contract limb.
- **C3 — the never-terminal limb.** Census of every run created in the whole
  session by status, at the end of the flood and again after a stated settling
  interval. Predict: Part A's 999 non-holder runs and Part B's probe are all
  still non-terminal. **Reported as "still non-terminal at T+N", never as
  "never"** — this session ends its own window.

### Part D — the free load generator

`POST /api/v1/agent-identities/agent2/credentials/revoke` (admin,
`server.go:411`), then a bounded window measuring agent2's request rate and its
Postgres cost, then re-enrollment or teardown.

**Prediction (AMENDMENT 5): ~0.5 req/s from the claim loop's fixed 2 s sleep,
not `FINDINGS.md:495`'s 8.3 req/s, and nothing backs it off** — no exponential
backoff, no give-up, no `Retry-After` honoured on this path. The cost per
rejected request is at least the credential lookup, i.e. a Postgres query per
rejection.

### Part E — retention

Untruncated survey with hit counts over `docs/*.md` excluding
`docs/superpowers/`, for the three env vars, the three flags, and the phrases
"keep forever" / "never trim" / "retention". **AMENDMENT 1 has already removed a
third of the expected finding; the survey result is reported either way.**

---

## RESULTS

*(appended after execution)*
