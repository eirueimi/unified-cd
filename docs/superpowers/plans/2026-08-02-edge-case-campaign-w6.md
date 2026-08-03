# Edge-Case Campaign: Wave W6 (Scale / Abuse) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Execute Wave W6 (scale / abuse) of the edge-case campaign — measure where unified-cd's
amplification, queueing and connection surfaces break under load, and settle the deferred
`LogPusher` disk-spill decision — recording findings.

**Architecture:** Same recording pattern as W0-W4. Runbooks under `test/edgecase/scenarios/`,
findings appended to `test/edgecase/FINDINGS.md`, raw captures to the session scratchpad and copied
to `<project parent>/edgecase-evidence/w6/` at the checkpoint. Environment is the `test/ha` compose
stack; **Kubernetes is deliberately out of scope for this wave** (see below).

**Tech Stack:** the `test/ha` compose rig, `test/edgecase/tools/inject.sh` and `w3/w3-4-logfault.sh`,
Postgres statement logging, the nginx access log, and new load/observation harnesses built in Task 1.

## Read this before planning anything

Reconnaissance (read-only, 2026-08-02) established four things that change the wave's shape. Each is
code-read unless labelled otherwise, and — per the campaign's five-wave record — **`file:line`
claims tend to hold while *mechanism* claims fail. Treat all of it as claims to check.**

### 1. W6-S2's charter asks for two numbers that cannot come from one run

`LogPusher` is `internal/agent/runner.go:207-446`. `flushLocked` (`:358-366`) issues **one
`AppendLogBulk` per pending batch per tick** — tick *k* costs *k* requests, cumulative triangular.
The O(t²) claim is **confirmed**, with one correction: **partial-progress accounting DOES exist at
batch granularity** (a batch that succeeds is dropped from `stillPending`). What is absent is
progress *within* a batch, and any coalescing.

Pending is capped at **1 MiB of line text only** (`:256`; `pendingSizeBytes` at `:438-446` sums
`len(r.Line)` and nothing else), evicted **drop-oldest**, and only while `len(p.pending) > 1` — the
newest batch is always kept even if it alone exceeds the cap. Two regimes follow:

- **Chatty step** (4 KiB write-path batches, `:255`): ceiling ~256 batches → per-tick cost plateaus
  around 256 requests, and *then* the drop path engages.
- **Sparse step** (~20-byte batches every 2 s): ceiling ~50,000 batches ≈ **28 hours** to reach — any
  realistic outage is purely quadratic and **never drops**, exactly as W1-6 observed
  (`grep -rc "dropped"` = 0).

**The two regimes are mutually exclusive.** You get the quadratic request curve **or** the drop
marker, essentially never both. The design doc's "drop-marker frequency and lost-line counts under
sustained load" reads as one workload; **it needs two.** A **partial/flapping** fault (one healthy
replica, nginx round-robin) drains stochastically and is **not** quadratic — a deliberate third arm,
not to be confused with the total-outage one.

**Never exercised, and the most important thing this wave can measure:** `StartAutoFlush` passes the
step context with no per-flush deadline (`:282`); the HTTP client timeout is **60 s**
(`internal/agent/client.go:53`). Under a **black-hole** partition (dropped packets, not RST/refused)
a single tick could block `len(p.pending) × 60 s` **while holding `p.mu`**, which blocks `Write` and
therefore the step's stdout pipe. W1-5 and W1-6 both used nginx 403s, which fail fast. *(Inferred.)*

**Observability, enumerated not asserted:** zero `slog`/`log` statements in the whole `LogPusher`;
the host agent exposes **no metrics endpoint at all**; `droppedLines` is unexported with no accessor.
The only signal is the marker at `:401-424`, **attempted only when `len(p.pending) == 0`** — i.e.
after the backlog fully drains. If the run ends with backlog outstanding the marker never fires and
the loss is permanently invisible. That is the interaction with the W1-2 major.

### 2. The dominant amplifier is the controller's per-line write path, not the LogPusher

`internal/controller/api_agent.go:721-734` loops calling `AppendLog` per line.
`internal/store/postgres.go:918-936` does one `QueryRow` **plus one `pg_notify` per line**, with no
transaction and no multi-row INSERT. A 2,000-line burst (`workloads/logburst.payload.json` emits
exactly 2,000) = **4,000 sequential pool acquisitions inside one HTTP request**, and 2,000 NOTIFYs.

Every NOTIFY wakes every SSE subscriber for that run (`internal/controller/sse.go:118-143`), and
each wake issues `TailLogs(..., 10_000)` **plus** a `GetRun`. Cost is **`2N + 2NS`** for S
subscribers. And `handleAgentLogBulk` decodes `r.Body` **unbounded** — the repo-wide enumeration of
`MaxBytesReader` returns exactly **one** hit, `internal/controller/api_webhooks.go:118` (webhooks
only).

**This needs no scale at all: one run and a statement-log capture.** Highest yield in the wave.

### 3. Capacity ceilings that bound what W6 can attempt

- **The rig executes exactly 2 concurrent real runs.** 2 agents × `MaxConcurrent` default **1**
  (`internal/agent/agent.go:218-221`); the compose file passes no `--max-concurrent`.
- **The synthetic-agent instrument is not committed.** `w3-5/synth.sh` and `w3-6/synth.sh` were
  session artefacts with hardcoded absolute paths; `test/edgecase/README.md:92-98` already flags
  promoting it as worth a task and it was never done. **It is a hard prerequisite** for the SSE
  harness, the claim long-poll harness, and any multi-producer W6-S2 arm.
- **Postgres `max_connections` is the stock default 100** (`test/ha/docker-compose.ha.yaml:3-13`, no
  override) against **128 listen-pool slots per controller** (`UNIFIED_DB_LISTEN_MAX_CONNS`; one
  connection held per live SSE stream, `internal/store/postgres.go:1665-1677`, pool cap
  `postgres.go:51`). **~100 concurrent SSE streams saturate Postgres.** Cross-reference
  `FINDINGS.md:43`: controllers **crash-loop with no DB-connect retry at startup**, so a controller
  restarting while PG is at `max_connections` may not come back.
- **nginx has no upstream `keepalive`** (`test/ha/nginx.conf`), so every proxied request opens a
  fresh TCP connection, and `events {}` leaves `worker_connections` at 512. **Any high-rate arm
  should run against a controller directly**; through the LB you measure the rig, not the product.
- **Do not plan a Kubernetes concurrency scenario.** `internal/k8sagent/config.go:97` defaults
  `MaxConcurrent` to **100** (negative = unlimited), but Docker Desktop Kubernetes co-resident with
  a 9-container compose stack will hit node resources and image-pull serialization first, and the
  failure will look like a `podStartTimeout` cascade already filed at `FINDINGS.md:2297`. **The
  100-vs-1 default asymmetry is worth filing as a code-read observation, not a scenario.**
- **W6-S1 must disclaim its sizing number.** A "~100-user operation" figure cannot be measured here,
  only extrapolated. Charter it as "find the first breaking point and its cause".

### 4. Retention defaults are effectively unlimited, and there is almost no rate limiting

`UNIFIED_AUDIT_RETENTION_DAYS`, `UNIFIED_RUN_RETENTION_DAYS`, `UNIFIED_LOG_TRIM_DAYS` all default to
**0 = keep forever** (`cmd/controller/main.go:203-205`). `internal/config/controller.go:96-115`
contains **no scale knobs at all**. **The only rate limiter in the product is the enrollment
limiter** (`internal/controller/enrollment_limiter.go:14`); there is no limiter middleware in the
stack (`internal/controller/server.go:285-293`). W6-S3's premise — demonstrate the absence of rate
limiting — is correct as written.

## Must-cite-never-re-file index

These are already filed. W6 will re-trigger several **on contact**; cite them, do not re-file.
Verify each line still resolves before citing — `FINDINGS.md` grew during W4.

- **`:465`** — the LogPusher O(t²) entry. It **explicitly hands the request-count-shape measurement
  to W6-S2.** W6-S2 produces the curve; it does not re-establish the mechanism.
- **`:1398` / `:1449`** — W2-9's `internal/controller/scheduler.go:58` 50-row Pending ceiling.
  `:1399` says explicitly it must not be double-counted. W6-S3 re-triggers it on every run.
- **`:779` / `:805` / `:807`** — W2-4, the queued-reaper window widening with backlog: **4 ms at 1
  run, 475 ms at 161, ~2.93 ms/run**, ~29 s at 10,000 labelled derived. `:810-812` lists **two
  still-open un-exercised limbs** (`cancelDescendantRuns` on a claimed descendant; `FinishRun`'s
  ungated lock releases) that a mutex-bearing W6 batch could legitimately file.
- **`:830` / `:865`** — the real deadline is the next 30 s sweep, not the grace; and the grace is
  measured from `created_at`, so a mutex-blocked run enters `Queued` with **zero** grace.
- **`:450` / `:495`** — a de-credentialled agent is a permanent ~8.3 req/s un-backed-off rejection
  source; `:495` **offers it to W6-S3 as a cheap sustained load generator.**
- **`:515` / `:563`** — ten of eleven "leader-elected" jobs are per-tick mutexes (2.15-2.40× measured
  on 3 replicas); three have no lock at all; the git resolver costs 5.0 queries/s **per replica on
  an idle stack**. **A load multiplier that applies to every W6 measurement — baseline it.**
- **`:1878` / `:1924`** — W3-5 seal drops. **`:474` and `:496` both warn: if a W6 fix recommendation
  generalises "coalesce the backlog" to an agent-level batcher it collides with this. The two
  changes must not land in either order without the other.**
- **`:1655`** — nginx 1 MiB body cap → 413. **`:2015`** — `RunCacheCleanup` 24 h hardcoded inline,
  first fire at t+24 h, so cache cleanup is effectively absent for any W6 session.
- **`:1069`** — one locked `schedules` row stops all scheduling cluster-wide; 51.3 s of `Pending`
  measured. A PG-saturating arm could reproduce this incidentally.
- **`:43`** — controllers crash-loop with no DB-connect retry at startup. **Do not file as new** when
  the connection-pressure arm triggers it.
- **`:496`** — the disk-spill gate statement. **The single most important paragraph for W6-S2.**

## Global Constraints

- All committed text is English (AGENTS.md).
- Work on branch `plan/edge-case-w6` in worktree `wt-edge-spec` — never commit on the main checkout.
- **No production-code changes.** Test-only files under `test/edgecase/` and docs. Do not modify
  `manifests/`, `test/ha/`, or `test/edgecase/workloads/podcap-job.payload.json`. Rig changes go in
  `test/edgecase/compose/` overlays (17 precedents; `dupagent.override.yaml:67-88` is the
  add-a-new-service shape).
- Classification: **violation** = contradicts an invariant (I1-I7) **by its own text**
  (`FINDINGS.md:1509`) **or** a statement in `docs/*.md`. NOT contracts: an inline comment inside a
  function body, an unexported helper's doc comment, anything under `docs/superpowers/`.
  **Observation** says "observation" in its **title** and repeats it in its **Severity** line.
- Every number traces to a capture whose window covers it. Label derived / inferred / code-read.
  Annotate uncaptured live observations. **Do not write "never" for a window you ended yourself.**
- **Never `head` a docs survey; always report the hit count.**
- **When you claim a class is fully enumerated, verify the enumeration.**
- **An arm is verified when some capture measures its effect**, not when its comment carries a
  measurement. Two verbs have shipped inert in this campaign while passing all their own state
  checks. **Any arm in front of a long-poll endpoint must sever established connections**, not
  merely block new ones.
- **Before filing, grep `FINDINGS.md` for the finding itself**, not just the doc text.
- Scrub credentials (`uca_`, `uce_`, `ucr_`, JWTs) from every capture. Kill every background process
  and capture its final output.
- **`FINDINGS.md` self-citations must keep resolving.** Appending is safe; if you edit above the
  append point, re-check every affected `FINDINGS.md:NNNN`.

---

### Task 1: The harnesses — this wave is instrument work first

**Files:** Create `test/edgecase/tools/w6/` (harnesses), `test/edgecase/compose/maxconcurrent.override.yaml`, a slow-trickle workload; Modify `test/edgecase/README.md`

Every W6 scenario is blocked on instruments that do not exist. Build them first and **prove each by
effect**, not by inspection.

- [ ] **Step 1: Promote the synthetic agent.** `w3-5/synth.sh` and `w3-6/synth.sh` are uncommitted
      session artefacts with hardcoded absolute paths; `test/edgecase/README.md:92-98` says a
      re-runner "must rebuild the helper from the runbook's own steps." Parameterise agent id, job
      name, and server; commit under `tools/w6/`. **Prerequisite for Steps 3, 4 and Task 4.**
- [ ] **Step 2: A concurrent load generator.** `tools/bulk-submit.sh` is a serial `curl` loop — it
      produces *depth*, not *rate*. Build one that holds N parallel in-flight requests against a
      **named controller** (not the LB) and records per-request start/end. Verify by effect: show N
      genuinely concurrent, not N sequential.
- [ ] **Step 3: An SSE subscriber harness.** Nothing in `tools/` opens or holds an SSE stream. Note
      `README.md:59-62`: an nginx reload severs in-flight SSE, and SSE captures must be taken
      **straight against a controller**, so the harness needs per-controller targeting.
- [ ] **Step 4: A PG connection sampler** — `pg_stat_activity` over time, by pool/application. Does
      not exist; W6-S1's core metric.
- [ ] **Step 5: A request-count-shape recorder.** W1-6 derived its numbers from
      `unifiedcd_agent_auth_events_total`, which only worked because every request was *rejected*.
      Under a transient fault the requests **succeed**, so that counter is useless. Use the **nginx
      access log** (precedent: the custom `logfault` format, `README.md:55-58`) or
      `unifiedcd_http_requests_total{route=".../logs/bulk"}` sampled per controller on a fine grid.
      **Say which and why, and show it resolves individual requests.**
- [ ] **Step 6: A slow-trickle workload** for the quadratic regime — `tick` (30 lines / 30 s) is too
      short. **Verify whether `longrun.yaml` exists** or only `longrun.payload.json` before planning
      around it. Validate every fixture through the real `dsl.Parse` with `KnownFields(true)` and
      **paste the output**.
- [ ] **Step 7: A `MaxConcurrent` overlay.** The agents are declared with an explicit `command:` list
      and no `--max-concurrent`; W3's `mixedkek` precedent says this is an overlay, never an edit to
      `test/ha/`. **Record that raising it changes what is measured** — N runs sharing one agent
      process's CPU and one `p.mu` per pusher is not N independent agents.
- [ ] **Step 8: Baseline the load multiplier.** `FINDINGS.md:515`/`:563` say the background jobs
      already cost measurable query volume on an *idle* stack (git resolver 5.0 q/s per replica).
      Measure the idle floor so every later number is net of it.
- [ ] **Step 9: Commit in increments; update `test/edgecase/README.md`.**

---

### Task 2: W6-S2a — the per-line write-path amplification

**Cheapest and highest-yield in the wave: one run and a statement-log capture. Run it first.**

**Files:** Create `test/edgecase/scenarios/w6-2a-log-write-amplification.md`; Modify `FINDINGS.md`

**Invariants:** likely none by its own text — expect a documented-contract limb or an observation.

- [ ] **Step 1: Write the runbook.**
  - **Part A — the base cost.** One `edge-logburst` run (2,000 lines). Measure the actual query
    count and `pg_notify` count against the predicted `2N`. Statement logging: **one `ALTER SYSTEM`
    per `psql -c`** (two in one is an implicit transaction, refused *silently* while
    `pg_reload_conf()` still returns `t`); verify `log_statement` **and** `log_line_prefix` in a
    **fresh** session on arm, and revert.
  - **Part B — the subscriber multiplier.** Repeat with S = 0, 1, 5, 10 SSE subscribers and measure
    against the predicted `2N + 2NS`. **Take SSE straight against a controller, not through the LB.**
  - **Part C — the unbounded body.** `handleAgentLogBulk` decodes `r.Body` with no `MaxBytesReader`.
    Establish what actually bounds it (nginx's 1 MiB default is already filed at `:1655` — cite it)
    and whether anything bounds it on a direct-to-controller path.
  - **Part D — enumerate the NOTIFY consumers.** Verify the enumeration rather than asserting it:
    what else listens, and what does each wake cost?
  - Recording: judge on the evidence. A measured super-linear cost with no configurable bound is a
    real finding; matching a documented promise is conformance and worth recording as such.
- [ ] **Step 2: Commit the runbook before executing.** **Step 3: Execute.** **Step 4: Findings,
      teardown, commit** (scenario id w6-2a).

---

### Task 3: W6-S2b — the `LogPusher` curve, and the disk-spill gate

**This is the wave's chartered obligation.** `FINDINGS.md:496` is the gate statement — read it first
and answer the question it actually asks.

**Files:** Create `test/edgecase/scenarios/w6-2b-logpusher-curve.md`; Modify `FINDINGS.md`

**Invariants:** I4, I5 — but only if contradicted by their own text.

- [ ] **Step 1: Write the runbook, with three arms and an explicit statement of why one run cannot
      produce both numbers.**
  - **Arm 1 — the quadratic regime (sparse workload, total outage).** Measure the **request-count
    curve**, not just a drop count. Predicted: triangular, no drops, marker never fires. Use the
    Task 1 Step 5 recorder. `tools/w3/w3-4-logfault.sh` is a URI-scoped fault on the log-bulk
    endpoint specifically and is the most directly reusable asset here.
  - **Arm 2 — the drop regime (chatty workload, total outage).** Predicted: per-tick cost plateaus
    around 256 requests, then drop-oldest engages. Measure the plateau and the drop marker. **State
    the two regimes' mutual exclusivity as a result**, since the charter assumed otherwise.
  - **Arm 3 — the flapping fault.** One healthy replica / stochastic drain. Predicted: not
    quadratic. This is the arm closest to a real outage.
  - **Arm 4 — the black-hole partition, never previously exercised.** W1-5/W1-6 used nginx 403s,
    which fail fast. Drop packets instead and measure whether a tick blocks
    `len(p.pending) × 60 s` **holding `p.mu`**, stalling `Write` and the step's stdout pipe. Note the
    agent image has no iptables and no `NET_ADMIN` (`FINDINGS.md` W1 records this), so say how you
    achieved a black hole — or that you could not, and what that leaves unmeasured.
  - **Part E — the marker's reachability.** The drop marker is attempted **only** when
    `len(p.pending) == 0`. Show what happens when a run ends with backlog outstanding, and connect
    it to the W1-2 major rather than re-filing it.
  - **Step 2: Answer the gate.** State plainly what the measurements imply for the disk-spill
    decision, including the constraint from `:474`/`:496` that a coalescing fix collides with W3-5's
    seal drops and the two must not land in either order without the other. **A recommendation is
    the deliverable here; hedging is not.**
- [ ] **Step 3: Commit runbook.** **Step 4: Execute.** **Step 5: Findings, teardown, commit**
      (scenario id w6-2b).

---

### Task 4: W6-S1 — connection pressure

**Files:** Create `test/edgecase/scenarios/w6-1-connection-pressure.md`; Modify `FINDINGS.md`

**Invariants:** I5 (bounded recovery), I7 (state display consistency) — by their own text or not at all.

- [ ] **Step 1: Write the runbook, chartered as "find the first breaking point and its cause",
      explicitly disclaiming any sizing number.**
  - **Part A — establish the ceiling empirically** rather than inheriting it: PG `max_connections`,
    the per-controller pool caps, and how many SSE streams it actually takes. The recon's 100 is
    read from config, not measured.
  - **Part B — what breaks first, and how it presents.** Does `/readyz` degrade? Do the background
    jobs starve before the API does? Does an SSE client past the cap get a 200 with backfill and
    then a silently dead stream (`sse.go:69-103` runs before `:118`, and `ListenForNotify` blocks on
    `conn.Acquire`)? **That last one is a concrete, cheap, unresolved question — settle it.**
  - **Part C — the recovery limb, and the dangerous one.** Restart a controller while PG is at
    `max_connections`. `FINDINGS.md:43` says controllers crash-loop with no DB-connect retry at
    startup — **cite it, do not re-file it** — and measure whether the stack recovers on its own or
    stays degraded. The compose file sets **no `restart:` policy** on the controllers.
  - **Part D — the claim long-poll surface.** Needs more agent identities than the rig's 2; use the
    Task 1 synthetic agent. Measure what N concurrent long-polls cost.
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings, teardown, commit**
      (scenario id w6-1).

---

### Task 5: W6-S3 — trigger flood and the absence of rate limiting

**Files:** Create `test/edgecase/scenarios/w6-3-trigger-flood.md`; Modify `FINDINGS.md`

**Invariants:** I1 (run accounting) — by its own text: *"every API-accepted run reaches exactly one
terminal state; no phantom runs from duplicate fires/webhooks"*
(`docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:48` — **quote it from the file, and
check the line; three W4 runbooks got this wrong and one quoted a clause that does not exist**).

- [ ] **Step 1: Write the runbook.**
  - **Part A — demonstrate the absence.** There is no limiter middleware
    (`internal/controller/server.go:285-293`); the only limiter is enrollment's. Demonstrating
    *absence* needs only enough rate to make the point — do not turn this into a throughput contest
    against nginx, which has no upstream `keepalive` and will hit TIME_WAIT exhaustion first (that
    would measure the rig, not the product).
  - **Part B — compounding with W2-9.** A backlog >50 saturates every Pending snapshot
    (`scheduler.go:58`) and newer runnable runs are never examined. **Cite `:1398`; `:1399` forbids
    double-counting.** Measure what the flood does to *end-to-end* latency for an unrelated run.
  - **Part C — I1 under the flood.** Does every API-accepted run still reach exactly one terminal
    state? This is the invariant's own text and the flood is exactly its stress case.
  - **Part D — the free load generator.** `FINDINGS.md:495` offers a de-credentialled agent as a
    permanent ~8.3 req/s un-backed-off rejection source. Use it or say why not.
  - **Part E — retention.** `UNIFIED_AUDIT_RETENTION_DAYS`, `UNIFIED_RUN_RETENTION_DAYS`,
    `UNIFIED_LOG_TRIM_DAYS` all default to **0 = keep forever**. Check what each is documented to do
    (untruncated survey with hit count) and whether "keep forever" is stated as the default anywhere
    operator-facing. A flood makes this consequential.
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings, teardown, commit**
      (scenario id w6-3).

---

### Task 6: The code-read observations, and the W6 checkpoint

**Files:** Modify `test/edgecase/FINDINGS.md`, `test/edgecase/README.md`, `<project parent>/edgecase-evidence/README.md`

- [ ] **Step 1: File the code-read observations this wave established but did not execute.** At
      minimum: the k8s agent's `MaxConcurrent` defaults to **100** with negative meaning unlimited
      (`internal/k8sagent/config.go:97`) while the host agent's defaults to **1**
      (`internal/agent/agent.go:218-221`) — an asymmetry with no documented rationale. Label
      code-read; do not dress it as a measurement. Add any other reconnaissance result that survived
      but had no scenario.
- [ ] **Step 2: Append `## Checkpoint: W6 complete`** in the W3/W4 format. State at minimum:
      (a) **the disk-spill recommendation**, plainly, as the wave's chartered deliverable;
      (b) **the two-regime result** and that the charter's single-run assumption was wrong;
      (c) what the harnesses cost and which survive for future work;
      (d) what this laptop could not measure, and what any number here is extrapolated *from*;
      (e) whether the "verified code facts" mechanism-vs-`file:line` pattern held for a **sixth**
      consecutive wave;
      (f) carry-forwards still open — `RunGitResolver`'s undemonstrated harm (W2-1), the W2-6 3-hour
      promotion-free schedule observation, W2-4's two un-exercised limbs (`:810-812`), and the
      campaign's invariant-set coverage gaps (no secret-store integrity clause, no cache-integrity
      clause);
      (g) whether W6 changes the **zero-criticals calibration**. The escalation set currently stands
      at **six, hubbed on W1-2 (`FINDINGS.md:179`)**. Do not manufacture a critical; do not avoid one.
- [ ] **Step 3: Hunt inconsistencies across the wave's documents and report every one** — never
      resolve one silently. W3's checkpoint found six; W4's found five and then reproduced the
      authoritative-document-stale failure mode twice itself. Look for that shape specifically.
- [ ] **Step 4: Archive** to `<project parent>/edgecase-evidence/w6/`, verify with `diff -r`, run a
      fresh credential sweep over the whole tree, and update both READMEs.
- [ ] **Step 5: Commit** (`test(edgecase): record W6 checkpoint`).
