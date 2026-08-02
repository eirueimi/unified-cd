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

**The answers, up front.** Nothing on the run-creation path is rate limited, and
the price is not the flood — it is what the flood leaves behind. One
**developer**-role PAT, 16 workers, **2.964 s**: 5,000 requests at **1,687
req/s**, **zero 429**, 2,241 permanent runs and 2,624 permanent audit rows
created, all three controllers' pools pinned at Postgres's `max_connections`,
and **55.18% of the flood's own requests rejected under three different status
codes, two of which are untrue**. `/readyz` read **200 in 87 of 87** in-window
replica-readings throughout. Three minutes after the client disconnected the
stack was still refusing connections, and for the **95 seconds** of flat
post-flood observation it ran at **1,071.6 statements/s — 14.9x the published
71.8 q/s idle floor — of which 93.3% was futile**: the scheduler re-attempting
50 known-blocked runs five times a second forever.

**I1 held on both clauses this scenario could newly test.** 9 distinct terminal
runs against 9 counted terminal transitions — **no double terminal write**. Four
independent counts of the flood's accepted runs agree exactly at **2,241**, and
the LB arm produced **400 rows for 400 requests** while **three of those requests
provably traversed two upstreams** — **no phantom runs**. The third clause,
"reaches ... a terminal state", is contradicted for **2,656 of 2,665** accepted
runs, but the mechanism is `scheduler.go:58` and `FINDINGS.md:1399` forbids
re-filing it; it is cited, not counted.

### Deviations from the runbook, stated before any number is read

1. **Part A ran `-n 5000`, not the planned `-n 1000`.** The calibration burst
   (`a-calibration.txt`) resolved 16 requests in **192 ms**, so 1,000 requests
   would have completed in well under a second — shorter than a single cell of
   the 2 s `w6-pgsample.sh` grid, which would have left the flood invisible to
   every recorder. 5,000 was chosen to span at least one grid cell. It spanned
   **2.964 s**, i.e. barely one and a half cells, so the flood itself is still
   resolved only by `loadgen`'s own per-request CSV and by the per-second
   statement histogram; the pgsample grid resolves the **aftermath**, which
   turned out to be the more important quantity.
2. **The 16 calibration requests are real runs and are in every census below.**
   They created the mutex holder and the first 15 blocked `Pending` rows, which
   is why the pre-flood statement rate is already **368 statements/s**, not the
   71.8 q/s idle floor. Every "idle floor" comparison below says which.
3. **A mid-execution hypothesis was formed and refuted.** On seeing saturation at
   only 16 in flight, the working hypothesis was `auth.go:79`'s undeadlined
   `TouchPAT` goroutine (AMENDMENT 3) amplifying one request into an unbounded
   background connection demand. **The statement census refutes it**:
   `UPDATE pats SET last_used_at` ran **1,632 times = 14.84/s**, 1.3% of the
   window's statements. It is recorded because it was wrong.

### Predictions versus outcome

| # | Prediction | Outcome |
|---|---|---|
| A(a) | **zero 429** | **HELD.** 0 of 5,000. No admission limiter exists on this path |
| A(b) | all accepted, no 5xx | **REFUTED.** 2,241 accepted (44.82%); 549 5xx (10.98%); plus 1,985 401 (39.70%) and 225 404 (4.50%) |
| A(c) | ~4 synchronous + 1 asynchronous statement per accepted run | **HELD.** 12,302 attributable statements / 2,241 accepted runs = **5.49** |
| A(d) | no saturation at 16 in flight; `/readyz` 200 | **SPLIT: saturation REFUTED, `/readyz` HELD.** Saturated from sample 3 of 29; `/readyz` 200 in 87 of 87 replica-readings, 81 of them at/near the client-backend budget |
| A(e) | permanent rows survive the flood | **HELD.** `runs` 3,056 kB, `audit_logs` 3,057 rows / 688 kB, and a **one-row** `mutex_holders` bloated to **776 kB** |
| B | the unrelated probe does not reach a terminal state in the window | **HELD.** `Pending` at **975.5 s** against a **0.807 s** median baseline |
| C1 | four independent counts all equal | **HELD.** 2,241 / 2,241 / 2,241 / 2,241 |
| C2 | no phantom through the documented LB config | **HELD, and non-vacuously** — 3 retries measured |
| C3 | flood + probe still non-terminal | **HELD.** 2,656 of 2,665 |
| D | ~0.5 req/s, no backoff, no give-up | **SPLIT: magnitude REFUTED, shape HELD.** **8.567 req/s** steady state — 17x the prediction, because an agent runs **17** claim loops, not 1 |
| E | audit retention defaults to 90, not 0 (AMENDMENT 1) | **HELD**, and the survey found **no operator-facing doc that contradicts the code**. Part E files nothing |

---

## Part A — the absence, measured, and the price

```
loadgen [w6-3-floodA] mode=sustained requested_c=16 requests=5000 wall=2.964s
  maxInFlight=16  meanInFlight=15.97  overlapWindow=2.962s (99.9% of wall)
  window   first_start=09:15:10.043 last_end=09:15:13.007
  status   200=2241 401=1985 404=225 500=549 errors=0
```

`maxInFlight=16` equals `-c 16` exactly, so neither of `loadgen`'s guards fired:
the rig did not serialise the client and the instrument did not invent overlap.
**1,687 req/s from one process against one controller, and not one 429.**

### The 5,000 requests decompose exactly, and the decomposition is the finding

The statement census (`a-idle-report.txt`) resolves every request to the point in
`createRunFromJob` at which it died, and the four numbers reconcile to the unit:

| Statement class | Count | Equals |
|---|---|---|
| `SELECT ... FROM pats WHERE token_hash = $?` | **3,015** | 5,000 - 1,985 (the 401s) |
| `SELECT ... FROM jobs WHERE name = $?` | **2,790** | 3,015 - 225 (the 404s) |
| `INSERT INTO runs(...)` | **2,241** | 2,790 - 549 (the 500s) |
| `INSERT INTO audit_logs(...)` | **2,624** | — |

So each rejected request is a request whose next statement **never reached
Postgres**, and the three failure points produce three different status codes:

- **1,985 x `401 unauthorized`** — `GetPATByHash` could not get a connection, so
  `auth.go:76` (`if err == nil && pat != nil`) falls through every remaining
  scheme and the response is indistinguishable from a bad token. **A valid
  developer PAT told 39.70% of the time that it is not valid.** This is
  `FINDINGS.md:2517`'s phenomenon, now reproduced at **16 in flight** rather than
  8-at-2,554-req/s. **Cited, not re-filed.**
- **225 x `404 job not found: edge-mutex-hog`** — for a job that exists and had
  just answered 2,790 lookups. `api_runs.go:49-52` maps **any** `GetJob` error to
  404 and discards it. **Not covered by `:2517`; filed below.**
- **549 x `500`** with the honest body
  `create run: create run: failed to connect ...: FATAL: sorry, too many clients already (SQLSTATE 53300)`,
  because `api_runs.go:84-86` surfaces `CreateRun`'s error verbatim. **One of the
  three failure points tells the truth, and it is the last one.**

### The price, stated three ways

**Per request.** 12,302 attributable statements for 5,000 requests = 2.46 per
request, **5.49 per accepted run** — against the code-read prediction of 4
synchronous + 1 asynchronous. AMENDMENT 3's asymmetry is confirmed:
`UPDATE pats SET last_used_at` ran 1,632 times (agent credentials would have run
it at most once per 5 minutes), but at 14.84/s it is **not** what saturated
anything.

**Per second, during.** Peak second `09:15:12` = **5,076 statements/s**, 70.7x
the idle floor.

**Per second, after — and this is the number that matters.** Per-second
histogram, never a span average:

```
second(UTC)              stmts mutexIns
2026-08-02 09:15:06        366       75    <- pre-flood, 15 blocked runs already
2026-08-02 09:15:09        369       75
2026-08-02 09:15:10       4554      161    <- flood
2026-08-02 09:15:12       5076      166
2026-08-02 09:15:13       1313      308
2026-08-02 09:15:17       1064      250    <- steady state, client gone
...
2026-08-02 09:16:55       1063      250
```

**95 post-flood seconds: mean 1,071.6 statements/s, min 1,042, max 1,158 — flat,
not decaying**, with the client disconnected 100 seconds earlier. That is
**14.9x** the 71.8 q/s idle floor, and `mutexIns` sits at **exactly 250.0/s**,
which is `scheduler.go:58`'s `limit = 50` times the scheduler's 5 ticks/s
(`RunScheduler`'s 200 ms default, `scheduler.go:24-26`). See Part B.

### What the absence costs, and what bounds it

**What a single low-privilege client can cost.** `requireMinRole("developer")`
(`server.go:359, :370`) is the floor for `POST /api/v1/runs`. A developer PAT —
the credential handed to anyone who may trigger a build — bought, in **2.964
seconds**: 2,241 permanent `runs` rows and 2,624 permanent `audit_logs` rows
whose deletion is opt-in and off by default (Part E); Postgres pinned at
`max_connections` for **at least 3 min 17 s** (still refusing at 09:18:30, the
last unattended reading before the operator intervened; **not waited out**, and
consistent with W6-1's pgxpool `MaxConnIdleTime=30m` default); and a permanent
14.9x load multiplier.

**What bounds it: nothing in the product.** The enumeration in AMENDMENT 2 is the
whole limiter inventory, and both entries are on agent-credential paths. There
is no per-principal quota, no per-job queue depth cap, no admission control, no
`Retry-After` anywhere outside enrollment. The only thing that slowed this client
down was **the cluster failing**: 55.18% of its own requests were rejected — and
that "limiter" sheds load from *every* client on *every* replica, not from the
one causing it, because the exhausted resource is shared. On this rig the budget
is 100 connections; on the repository's own `docker-compose.yaml:30`
(`max_connections=1000`) the same shape needs proportionally more load, and
`docs/operations.md:173` documents that number for operators.

**What did NOT break, and it matters.** `/readyz` and `/healthz` read **200 on
every replica at every one of the 29 sample instants** — 81 of the 87
replica-readings taken at or above `max_connections - 3`. This is the pairing
`FINDINGS.md:2517` could not make, taken **in-window** by `w6-pgsample.sh -p`,
and it confirms W6-1's result rather than extending it. **An operator watching
the documented health surface (`docs/operations.md:154`) sees nothing.**

**One refinement to W6-1's "background jobs do not starve first".** W6-1 measured
zero controller `ERROR` lines across two saturated arms. This session logged
**three across the whole run**, two of them genuine background-job failures under
saturation:

```
09:15:46 controller1 ERROR queued-run reaper list error ... FATAL: sorry, too many clients
09:18:46 controller3 ERROR log archiver error ... FATAL: sorry, too many clients already (SQLSTATE 53300)
```

Two lines for roughly three and a half minutes of total DB unavailability. So
background jobs **do** complain — at a rate of about one line per two minutes of
outage, which is close enough to silence that W6-1's conclusion stands. Recorded
because "zero" is now known to be "nearly zero", and a future wave should not
treat an ERROR line here as a new phenomenon.

### What actually exhausted the pool is NOT measured, and this runbook does not guess

`w6-pgsample.sh` could not connect for **27 of its 29 samples** (recorded as
`UNAVAILABLE`, which is a saturation reading and not a gap), so the per-pool peak
table below is built from the **two pre-flood samples only** and says nothing
about the saturated state:

```
controller1  query idle peak=20   controller1 lock idle peak=7
controller2  query idle peak=15   controller2 lock idle peak=7
controller3  query idle peak=14   controller3 lock idle peak=7
TOTAL backends: min=76 max=76 mean=76.0 over 2 samples
```

**76 backends before the flood**, against the published 73-74 idle floor —
already elevated by the 15 blocked calibration runs. Which pool crossed the line
is **unmeasured**; the instrument that would answer it is the one the saturation
disables. What is measured is that **16 in flight on `POST /api/v1/runs`
saturated where W6-1's 8 workers at 20 req/s on a `GET` did not**, and that the
flood's own backlog fed back into the load *within the flood* — `mutexIns` had
already climbed from 75/s to 308/s by second 09:15:13.

---

## Part B — the unrelated run, and `FINDINGS.md:1398` on contact

| | Baseline (quiet stack) | Behind the flood |
|---|---|---|
| trigger -> terminal | **0.313 s, 0.856 s, 0.807 s** (median 0.807 s) | **not terminal at 975.5 s** |
| ratio | — | **>= 1,209x the median**, >= 1,140x the slowest baseline |

The probe was triggered at `09:21:16`, six minutes after the flood ended, on a
stack with **an idle agent long-polling for exactly this work**. The mechanism is
`scheduler.go:58`, confirmed directly rather than inferred:

```
oldest50_jobs: edge-mutex-hog          <- all 50 candidates are the blocked job
probe_rank_among_pending=2256          <- the probe is the NEWEST of 2,256 Pending
pending_total=2256
probe_status=Pending
```

**The blockage is self-sustaining, which `:1398` did not have to establish.** The
mutex holder rotated during the observation — run `a54a54a8` on `agent2` at
09:21:16, run `9237e5dd` on `agent1` at 09:25:35, and two `edge-mutex-hog` runs
reached `Succeeded` by teardown. Each holder occupies the mutex for its `sleep
600`, so the backlog drains at one run per 600 s: **2,254 remaining runs is
~15.7 days (derived from the fixture's own sleep, not measured)**, during which
every newer run of every other job is invisible to the scheduler. **The 400
`edge-w6-probe` runs Part C2 created are also all `Pending`** — the starvation is
global, not scoped to the blocked job.

**Zero log lines**, exactly as `:1398` recorded. The one surface that does move is
`unifiedcd_runs_current{status="Pending"} 2655` — the *backlog* is observable on
`/metrics`; the *starvation* and the churn are not.

**`FINDINGS.md:1398` is cited and NOT re-filed.** What Part B adds is the operator
number `:1398` lacks: **>= 1,209x**, on a stack with a free agent.

---

## Part C — I1 under the flood

### C1 — accounting reconciliation. Four counts, one number

| Count | Source | Value |
|---|---|---|
| 2xx responses | `loadgen` per-request CSV | **2,241** |
| `unifiedcd_runs_created_total{trigger="api"}` delta | sum over 3 replica registries (23 -> 2,264) | **2,241** |
| `runs` rows in `[09:15:10.000, 09:15:13.100]` | `psql` | **2,241** |
| of those, `job_name='edge-mutex-hog'` | `psql` | **2,241** |

`total_runs = 2264`, `distinct_ids = 2264` — **no duplicate ids, no extra rows,
no lost accept.** The same reconciliation was repeated after a controller restart
*and* a SIGKILL: post-restart `runs_created_total` summed to **401** across the
surviving registries against **401** rows (C2's 400 plus Part B's probe).

### C2 — the phantom mechanism the product documents, and it does not fire

400 POSTs through **nginx on :18080** with `controller2` SIGKILLed for the whole
window. All 400 returned **200** — the documented failover works.

```
pre-count  edge-w6-probe rows: 1
post-count edge-w6-probe rows: 401     -> 400 created for 400 requests sent
access-log rows inside loadgen's window: 400
ustatus distribution:  397 x "ustatus=200"   3 x "ustatus=504, 200"
```

**`rows == requests_sent` exactly.** And the arm is **not vacuous**: three
requests carry two upstream statuses, i.e. nginx really did pass them to a second
controller, and each still produced exactly one run.

```
1785662794.300 ... 200 ustatus=504, 200 rt=2.005 urt=2.001, 0.003 "POST /api/v1/runs HTTP/1.1"
1785662802.389 ... 200 ustatus=504, 200 rt=2.004 urt=2.000, 0.003 "POST /api/v1/runs HTTP/1.1"
1785662810.475 ... 200 ustatus=504, 200 rt=2.005 urt=2.000, 0.004 "POST /api/v1/runs HTTP/1.1"
```

**What kind of retry these were, stated because it bounds the claim.** nginx's
error log names them:

```
2026/08/02 09:26:34 [error] ... upstream timed out (110: Operation timed out) while
  connecting to upstream, ... upstream: "http://172.20.0.5:8080/api/v1/runs"
```

`while connecting` — the request body had **not** been sent, which is the case in
which retrying a POST cannot duplicate anything. (`172.20.0.5` is the address the
killed `controller2` held: container IPs rotated at the 09:18:55 restart and
`controller2` reclaimed `.5` when it was restored, with `controller1`/`3` holding
`.7`/`.6` — **inferred from the restore reading, not observed at kill time**. A
lesson for the harnesses: `w6-idleload.sh` and `w6-pgsample.sh` resolve the
IP-to-service map **once at startup**, and a container restart invalidates it.)

**The response-level case — an upstream that answered 502/503/504 *after*
receiving the body — was NOT reached, and by nginx's default should not be**:
non-idempotent methods are not passed to the next server unless `non_idempotent`
is listed, and neither `test/ha/nginx.conf:24-26` nor
`docs/high-availability.md:260-261` lists it. **So the phantom limb is closed for
the documented configuration, and it is closed by an nginx default that the
documentation never mentions.** Filed as a minor observation below.

### C3 — the never-terminal limb

Final census at teardown (`09:37:32`), 22 minutes after the flood:

```
accepted_total=2665   non_terminal=2656   distinct_terminal=9
probe_status=Pending age_s=975.5
```

**9 distinct terminal runs against 9 counted terminal transitions** — 7 before the
09:18:55 restart (`runs_finished_total` summed 1+2+2+2) and 2 after (1+0+1).
`unifiedcd_runs_finished_total` increments **only** on a successful CAS
(`internal/metrics/store.go:38-44`), so equality is a direct double-write
detector and it reads clean. **I1's "exactly one terminal state" clause HOLDS by
measurement, not by code-read.**

**2,656 of 2,665 API-accepted runs had reached no terminal state when this
session ended its own window.** The phrasing is deliberate: this session chose
when to stop. What can be said without over-claiming is that the newest of them
**cannot** be examined by the scheduler while 50 older blocked runs exist, which
is `scheduler.go:58` and is already filed at `FINDINGS.md:1398`. **Cited, not
re-filed, and not counted toward this scenario's tally.**

---

## Part D — the free load generator, and `FINDINGS.md:495`'s number is right for the wrong reason

Both windows are 60 s / 180 s of foreground wall clock, measured off the nginx
access log filtered to `agent2`'s IP and cross-checked against the controllers'
own counters.

| | credentialled (control) | de-credentialled (fault) |
|---|---|---|
| window | 60.056 s | 179.3 s |
| requests | 38 | **1,457** (1,440 x 401, 17 x 200) |
| rate | **0.633 req/s** | **8.13 req/s** mean, **8.567 req/s** over the last 30 s |
| amplification | — | **13.5x** |

**Cross-instrument agreement**: the nginx count of 1,440 401s equals the sum of
`unifiedcd_http_requests_total{code="401",route=".../claim"}` across the three
replicas (481 + 481 + 478 = **1,440**).

**The mechanism, and it is 17 loops rather than 1.** AMENDMENT 5 predicted
~0.5 req/s from a single claim slot. An agent actually runs **1 normal claim loop
plus a detached pool that defaults to 16** (`internal/agent/agent.go:310-321`:
`d := a.MaxDetachedConcurrent; if d == 0 { d = 16 }`), which the agent's own log
confirms — `slot` 0-15 with `"detached":true` and `slot` 0 with
`"detached":false`. So:

- **credentialled**: 17 loops x one request per **30 s** long-poll = 0.567 req/s
  (measured 0.633);
- **de-credentialled**: the 401 returns in `rt=0.001`, the long-poll no longer
  holds, and each loop falls to the error path's **fixed
  `time.After(2 * time.Second)`** (`agent.go:416-422`) = 17 / 2 = **8.5 req/s**
  (measured 8.567 over the last 30 s).

**So `FINDINGS.md:495`'s ~8.3 req/s is numerically confirmed and mechanistically
wrong.** `:495`'s figure is the `LogPusher`'s re-flush floor for an agent
executing a step; this agent was idle and had no pending batches. The two
arithmetics land in the same place by coincidence.

**Nothing backs it off, and the direction of the drift proves it.** First 30 s =
**5.733 req/s**, last 30 s = **8.567 req/s** — the rate **rises**, because the 17
loops were each mid-long-poll when revocation landed and converted to the 2 s
cadence at staggered times over the first ~30 s. The modal inter-request gap is
`0.000 s` (356 occurrences: the loops fire in bursts), the per-second histogram is
bimodal (70 seconds at 4 req/s, 70 seconds at 13 req/s), and the last 401 lands
at `09:33:30`, the final second of the window. **No exponential backoff, no
ceiling, no give-up** — note the contrast with `internal/agent/retry.go:33-36`,
which abandons permanently on any status < 500; the claim loop does not use it,
so the same binary contains both failure modes of the same class.

**The cost.** `agentAuth` re-reads the credential row from Postgres on **every**
agent request with no server-side cache (`internal/controller/agent_auth.go:62-77`,
as `FINDINGS.md:369` established), so 8.567 req/s is 8.567 wasted queries/s —
**~11.9% of the entire 71.8 q/s idle floor, per revoked agent, indefinitely** —
plus one `ERROR` line per attempt in the agent's log, which is itself unthrottled.

**All 17 non-401 responses were pre-revocation, and this was checked rather than
assumed.** Each has `rt=30.02`, and completion minus `rt` places every start
before `09:30:30.867`. **No revoked credential ever authenticated.**

---

## Part E — retention. The premise was wrong and the docs are right

The survey ran untruncated over the **17** `docs/*.md` files outside
`docs/superpowers/`. Hit counts (lines):

| Pattern | Hits | Pattern | Hits |
|---|---|---|---|
| `UNIFIED_AUDIT_RETENTION_DAYS` | 3 | `keep forever` | 3 |
| `UNIFIED_RUN_RETENTION_DAYS` | 3 | `never trim` | 4 |
| `UNIFIED_LOG_TRIM_DAYS` | 3 | `retention` | 22 |
| `audit-retention-days` | 3 | `retain` | 1 |
| `run-retention-days` | 4 | `unbounded` | 1 |
| `log-trim-days` | 4 | `grow` | 3 |

**Every operator-facing statement is accurate**, including the one AMENDMENT 1
predicted would be wrong:

- `docs/audit.md:169` — *"| `--audit-retention-days` | `UNIFIED_AUDIT_RETENTION_DAYS` | `90` | `0` disables cleanup"*. **90**, matching `main.go:29-34`.
- `docs/configuration.md:83` — *"Default `90`. `0` = keep forever."*
- `docs/configuration.md:84` — *"`0` (default) keeps runs forever."*
- `docs/configuration.md:85` — *"`0` (default) never trims."*
- `docs/operations.md:47` — *"By default unified-cd keeps every run forever: `runs` rows, log rows, archived logs, and artifacts all accumulate."*

**"Keep forever" is stated as the default, operator-facing, in the operations
guide, in the words the code uses.** There is no documented promise to
contradict, so **the violation limb the brief anticipated does not exist and Part
E files nothing.** The survey is the evidence and is kept as a negative result.

The consequence is still worth stating without a finding attached: the flood's
2,241 runs, 3,057 audit rows and 3,056 kB of `runs` are, under stock
configuration, permanent by design and by documentation. Only `audit_logs` has a
non-zero default (90 days), and it is the smallest of the three.

---

## Instrument defects found

**`w6-idleload.sh`'s revert fails exactly when its own window saturates
Postgres — the promise "always reverts" is conditional and was not.** The tool
header and `README.md` both say it "always reverts"; here it exited **2** with:

```
== revert ==
psql: error: connection to server ... FATAL:  sorry, too many clients already
```

and **left `log_statement=all` and the modified `log_line_prefix` armed on the
running cluster**, where every subsequent arm would have been recorded at full
volume. This is the same class of defect W6-1 fixed in `w6-pgsample.sh -p` (which
survived this window and recorded its own refusals as `UNAVAILABLE` readings),
and it is the *inverse* of the W6-2b lesson: there the setup verification was
consumed by its own probe; here the **teardown** could not run because the
condition under test denied it the resource. The capture itself is intact — 40.6
MB, 361,895 lines, with a `-window.txt` sidecar bounding it — so no number moves;
only the cluster state was left dirty. Reverted manually (`a-recovery.txt`), and
note that the revert needs **one `ALTER SYSTEM` per `psql -c`**: three statements
in one `-c` fail with `ALTER SYSTEM cannot run inside a transaction block`, which
the tool's own revert path would also hit. **No product code, `manifests/`,
`test/ha/` or `workloads/` file was changed by this scenario, and the harness is
recorded rather than patched — fixing it is a W6 harness task, not this
scenario's.**

**A second, smaller one**, already noted in C2: both `w6-idleload.sh` and
`w6-pgsample.sh` resolve container IP -> service name **once at startup**. A
container restart between arms silently invalidates the map, and this session had
one. Any per-replica attribution taken across a restart should be re-derived.

---

## Findings filed

Five entries, **all observations, no violations**: 3 major, 2 minor. Every one
was grepped against `FINDINGS.md` for the finding itself before filing.

| # | Title | Severity |
|---|---|---|
| 1 | a connection-pool failure is reported as `404 job not found` for a job that exists | major (observation) |
| 2 | no rate limit of any kind on run creation; the price of the absence | major (observation) |
| 3 | the scheduler re-attempts unqueueable `Pending` runs 5x/s forever with no backoff | major (observation) |
| 4 | the documented `proxy_next_upstream` list is inert for POST, and the docs do not say so | minor (observation) |
| 5 | a de-credentialled idle agent is a permanent 8.567 req/s source with no backoff | minor (observation) |

**Not filed, deliberately:** the 401-on-a-valid-token mechanism
(`FINDINGS.md:2517`); the 50-row scheduler ceiling and the starvation it causes
(`FINDINGS.md:1398`, and `:1399` forbids double-counting it); the pools' failure
to release promptly (W6-1 / `:2517`); the agent's 4xx-permanent-abandonment bug
(the W1-5 major), which finding 1 compounds with and cites.
