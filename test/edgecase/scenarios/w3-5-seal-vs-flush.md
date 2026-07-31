# W3-5 — the archive seal racing the final log flush

**Wave W3, Task 6.** The scenario attacks a loss channel in which the agent's
bytes **do** reach the controller, **are accepted with a `204`**, and are then
**discarded server-side** because the run's logs have already been sealed by the
archiver. The agent is told it succeeded. The only record anywhere is a
controller-side `slog.Warn`.

---

## The W1 boundary, stated before anything else

**W1-6 and the W1-2 major are a different defect and this scenario must not
re-measure them.** Those measured loss where the agent **never got its bytes to
the controller**: the step-end `Flush` gets a fixed 5 s retry budget
(`internal/agent/runner.go:343`, three 1 s-spaced retries under a 5 s cap), and
when that budget expires the unflushed tail dies inside the agent process, with
no `[N log line(s) dropped: controller unreachable]` marker
(`FINDINGS.md` W1-2 major entry; reproduced again in W1-6).

**W3-5 is the opposite half of the wire.** The bytes arrive. `handleAgentLogBulk`
runs. `AppendLog` executes an `INSERT` that matches zero rows because a
`run_log_archives` record exists. The handler returns **204**. Nothing is stored.

Different mechanism, different evidence, different fix:

| | W1-2 / W1-6 | W3-5 (this scenario) |
|---|---|---|
| Where the bytes die | in the agent's `p.pending`, at `Flush`'s 5 s exit | in the controller's `INSERT`, after a successful HTTP round trip |
| What the agent sees | a transport error, then give-up | **`204 No Content`** |
| Trace left | none agent-side, none controller-side | a controller `slog.Warn` |
| Fix shape | give `Flush` the `retryUntilSuccess` treatment | make the drop visible to the sender / to the run |

**If a run in this scenario also loses a step-log tail to the 5 s budget,
attribute that half to W1-2/W1-6 by citation and do not count it here.** Part B
runs a real agent through a partition and is the arm most likely to produce
both; it must separate them line by line.

---

## The vehicle, and the fact that killed the planned one

**The structural race this scenario was chartered to produce cannot be produced
on this rig.** The plan's W3-5 block
(`docs/superpowers/plans/2026-07-30-edge-case-campaign-w3.md:107-115`) rests on
sidecar logs being flushed by `CloseScopes` **after** `FinishRun`, by
construction, on every run. That ordering is real — `defer b.CloseScopes(...)` is
registered at `internal/agent/orchestrator.go:209` and therefore runs after
`FinishRun` at `:787-788` — but **this rig has no sidecars**, by two independent
mechanisms Task 3 established and `test/edgecase/README.md` records:

- the campaign envelope is `native: true`, which leaves `pod == nil`
  (`internal/agent/agent.go:593-623`), and `hostBackend.SetMasker` builds the
  sidecar pump only when `b.pod != nil`
  (`internal/agent/backend_host.go:362-368`);
- dropping `native: true` does not help — the claim then needs a container
  runtime and the agent containers have no Docker socket. (`test/ha/agent.Dockerfile`,
  which both the plan and the task brief assumed, **does not exist**; the image
  is built from `docker/agent.Dockerfile`.)

Every **remaining** agent-side flush point flushes *before* `FinishRun`:

| Source | Flush point vs `FinishRun` (`orchestrator.go:787-788`) |
|---|---|
| Main step stdout/stderr | per step, `finish()` from `StepLogWriters` (`backend_host.go:382-386`) |
| Post-hook output | `finishPostLogs(hookCtx)`, `orchestrator.go:706` |
| `finally` steps | `RunPipeline(finallyCtx, ...)`, `orchestrator.go:727` |
| ~~Sidecar logs~~ | ~~`CloseScopes`, after `:787`~~ — **unavailable on this rig** |

**So no arm of this scenario is the plan's structural, every-run race, and every
entry must say so.** What the arms are instead:

- **Part A — real seal, synthetic sender.** The seal is produced by the **real
  archiver on its real 30 s tick** against a genuinely `Succeeded` run; only the
  identity that pushes the late lines is synthetic (a curl-driven agent, the
  W3-6 instrument). The `POST .../logs/bulk` lands on the same handler, passes
  the same guard, and reaches the same `AppendLog` as an agent's.
  **This is a demonstration of the code path, not a natural race** — a real
  agent on this rig has no flush point late enough to reach it.
- **Part B — real agent, real seal, injected partition.** The one arm where
  *nothing* about the sender is synthetic. A partitioned agent's `p.pending`
  batches are re-offered after heal, by which time the run has been cancelled
  and sealed. This is a **natural race** if it lands, and it is the shape
  `docs/troubleshooting.md:865` names verbatim as a cause. It is capped and may
  not land; report the attempt count either way.
- **Parts C, D, E** — attribution, asymmetry, contract. C is instrument-driven,
  D is code-read, E is a docs survey.

**The README's standing advice is that a post-`FinishRun` flush "must seal by
hand (`INSERT INTO run_log_archives`)".** Part A does **not** do that: it lets
the archiver seal naturally and moves the synthetic half to the *sender*, which
is strictly stronger evidence (a hand-planted row proves the guard reads a row;
a real archive proves the archiver plants one that the guard then reads). **No
arm of this scenario writes a `run_log_archives` row by hand.** Say so, because
a reader who knows the README will expect otherwise.

---

## Corrections to inherited facts, established BEFORE execution

Per the W1 carry-forward rule the plan's "Verified code facts" block is a set of
**claims**. Every row of §"Verified mechanism" was re-read at this branch's HEAD.
**One substantive correction, two span corrections; the conclusions survive but
Part D's framing changes.**

- **CORRECTION 1 — SUBSTANTIVE. The brief's and the plan's sharpest framing is
  half wrong: the log endpoints get the *same* `rejectTerminal=false`
  accommodation the sidecar-status endpoint does.**
  The plan (`:108`) and the task brief say sidecar *status* was given
  `rejectTerminal=false` to accommodate the post-`FinishRun` window while
  sidecar *logs* "get none". At HEAD **all three** endpoints pass `false`:
  `handleAgentLogAppend` at `api_agent.go:551`, `handleAgentLogBulk` at
  `api_agent.go:709`, `handleAgentSidecarStatus` at `api_agent.go:760`. So a
  post-terminal log line is **not** rejected by the terminal guard — it is
  rejected later, by the **seal**, which is a different and strictly later gate
  (terminal at `FinishRun`; sealed at the archiver's next tick, ~30 s after).
  **The asymmetry is real but must be restated:** sidecar status is `UPSERT`ed
  and survives the whole window; log lines survive the terminal gate and die at
  the seal gate ~30 s later. Part D files this shape, not the brief's.
- **CORRECTION 2 — the sidecar-status accommodation comment spans
  `api_agent.go:741-752`, not `:744-748`.** The plan and brief cite `:744-748`,
  which names only the middle of it. The comment's load-bearing sentence for
  this scenario is at `:745-747` ("both agents stop their sidecar pumps via a
  deferred CloseScopes that runs AFTER FinishRun"), and its closing justification
  at `:750-752` ("UpsertSidecarStatus is a display-only upsert keyed by
  (run, index), so a late/duplicate write here is harmless") is what makes the
  contrast with logs sharp. **Quote it whole.**
- **CORRECTION 3 — the bulk handler's span is `api_agent.go:719-738`, and the
  warning is at `:736`, not `:737`.** The plan cites `:719-737` for the block and
  `:732` for "logs only the last run id seen". `:732` is right —
  `droppedRun = req.RunID` inside the loop — but the `slog.Warn` itself is
  `:736`, inside the `if dropped > 0` block at `:735-737`, and the handler's 204
  is `:738`.
- **CONFIRMED, not corrected:** the guard is inside the `INSERT`
  (`internal/store/postgres.go:918-936`; the `WHERE NOT EXISTS` at `:922`,
  `pgx.ErrNoRows → return 0, nil` at `:927-928`); the single-line drop path is
  `api_agent.go:564-570` with the 204 at `:570`; `CreateLogArchive` is
  `postgres.go:1519-1528` and its only caller is `archiveRunLogs`
  (`archiver.go:106`); the archiver's interval is hardcoded `30*time.Second` at
  `cmd/controller/main.go:400`.

---

## Invariants

Quoted verbatim from `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md`.

- **I4 (log/artifact integrity)** — `:51`:

  > "**Log/artifact integrity** — a Succeeded run's log line count matches what
  > the workload emitted; no duplicates, no reordering; archives stay readable"

  **This is the scenario's primary invariant and the fit must be argued clause by
  clause, not asserted.**
  - *"a Succeeded run's log line count matches what the workload emitted"* — this
    is the clause in play, and **it is why Part A must produce a `Succeeded`
    run.** Part B's run is `Cancelled`, so this clause does not literally reach
    it; Part B rests on the same mechanism but must not claim the clause.
  - *"no duplicates, no reordering"* — not touched. Nothing here duplicates or
    reorders. (W3-4 owns duplication.)
  - *"archives stay readable"* — **not touched, and do not stretch it.** The
    archive here is perfectly readable; it is simply short. W3-2's binding ruling
    on the mirror-image case is the precedent for reading this clause narrowly.

  **The complication that must be faced, not dodged:** in Part A the lines are
  emitted by the *instrument*, not by a workload. Argue the fit honestly — the
  invariant's subject is what reached the controller for that run versus what the
  run's log ends up containing — and if the honest reading is that Part A's
  numbers do not satisfy "what the workload emitted" literally, **say so** and
  rest the entry on the mechanism plus the contract limb.
- **I7 (state display consistency)** — `:54`:

  > "**State display consistency** — run status, approval status, and audit rows
  > never contradict each other or reality"

  **In scope only if a surface is found that lies about the drop.** Candidate:
  the run's archive record (`run_log_archives.line_count`) versus the number of
  lines the controller actually accepted `204`s for. Decide deliberately; a
  record that is *correct about the archive* is not a lying surface just because
  the archive is short.
- **NOT I1.** Every run reaches exactly one terminal state, including the
  synthetic ones (finished through `POST /agents/{id}/runs/{runId}/finish`).
  **State it with a census.**
- **NOT I2.** No step side effect is executed at all in Parts A/C (the probe
  steps never run) and Part B's workload has no side effect beyond stdout.
- **NOT I3.** No mutex, semaphore or concurrency slot in any fixture used. The
  archiver's advisory lock (`logArchiverLockKey`, `archiver.go:15`) is not I3's
  subject — I3 is about *workload* locks verified via `mutex_holders` /
  `named_lock_slots`.
- **NOT I5.** Part B injects a partition and heals it, and the system does return
  to steady state — but the **lost lines never come back**, and that is a
  data-integrity fact, not a recovery-bound one. **Do not stretch "bounded
  recovery" over permanently discarded data**; W1-6 already drew that line.
- **NOT I6.** No run is terminalised out from under an agent that then keeps
  executing side effects. Part B *does* terminalise a run under a partitioned
  agent, but what is measured is the agent's **log delivery**, not its
  containment; the containment question was measured in W1-5.

### The contract limb — and this scenario's hardest judgement

**The behaviour under test is EXPLICITLY DOCUMENTED, in two places, and the
entry must lead with that rather than bury it.**

> "Log lines arriving after archival are discarded with a controller warning
> (`dropping log line for sealed run`) to keep the archive consistent; storing
> them would make the run untrimmable and, after trim, invisible."
> — `docs/operations.md:51`

> "An agent sent log lines for a run whose logs were already archived (~30 s
> after the run finished). … The archive is the sealed source of truth, so the
> lines were discarded"
> — `docs/troubleshooting.md:859`, whose "Fix" section (`:863-868`) calls
> occasional occurrences "expected noise" and lists as a common cause
> "Teardown/buffer flushes arriving later than the archiver delay" (`:866`)

**So the drop itself is sanctioned and cannot be filed as a contradicted
contract.** The campaign's rule makes an invariant contradiction sufficient on
its own (`FINDINGS.md:479`), so an I4 filing does not *require* a broken
promise — but an entry that ignores an explicit doc sanction is dishonest.
**Lead with the sanction, then argue what the docs do NOT say**, which is where
the finding lives:

1. Nothing anywhere says the **sender is told it succeeded**. `:859` says the
   lines "were discarded" and that the controller warns; it does not say the
   agent receives `204` and clears its buffer, so no retry, no marker, no
   agent-side trace ever exists. Contrast the *other* loss channel, whose
   marker is documented as a deliberate visibility feature:
   > "Rather than let that gap in the run's log pass silently, the agent counts
   > every discarded line and, on the next flush that successfully reaches the
   > controller, emits this one synthetic line reporting exactly how many lines
   > were lost."
   > — `docs/troubleshooting.md:895-898`

   **That is the loud-loss convention W1-2 filed against, and it is absent
   here** — and its absence here is *invisible to the agent*, which is a
   stronger statement than W1-2's (there the agent knew and stayed quiet; here
   the agent does not know).
2. Nothing says the **run's own log** is permanently short. `:918-919` promises
   the marker "is the only remaining record" for the *other* channel; for this
   channel there is no record in the run at all.
3. Nothing bounds or attributes the **mixed-batch** case (Part C).

**Check whether this branch has already ruled on every passage before citing
it** (the Task-3 lesson). `docs/operations.md:51` and
`docs/troubleshooting.md:889-899` are both already cited by
`w3-4-bulk-append-duplication.md:205-216` and by `FINDINGS.md:1561`, which read
`:889-899` narrowly — as *authorising retry* and saying nothing about what the
controller already committed. **That narrow reading is binding and this
scenario must stay consistent with it.** Run:

    grep -rn "operations.md:51\|troubleshooting.md:85\|troubleshooting.md:86\|troubleshooting.md:87\|troubleshooting.md:88\|troubleshooting.md:89" test/edgecase/

and record the result **before** filing.

### Contract survey — run IN FULL and print the hit count

W3-3 lost a contract violation to a `head`-truncated survey; the method rule is
binding. Capture untruncated, with `| wc -l` next to each:

    grep -rn -iE "archiv|seal" docs/*.md
    grep -rn -iE "log line|dropped|lost log|log loss|silently" docs/*.md
    grep -rn -iE "204|no content|acknowledg|ack\b" docs/*.md
    grep -rn -iE "at-least-once|at least once|exactly once|guarantee" docs/*.md

Read the surrounding section before citing anything (the W2-7 lesson).

---

## Verified mechanism — read this before designing anything

Every row re-read at this branch's HEAD; the `file:line` is the claim.

| # | Fact | Site |
|---|---|---|
| 1 | **The seal is the mere existence of a `run_log_archives` row.** No boolean, no `sealed_at` column consulted | `postgres.go:922` (`WHERE NOT EXISTS (SELECT 1 FROM run_log_archives WHERE run_id = $1::uuid)`) |
| 2 | **The guard is INSIDE the INSERT**, so it is atomic per line and costs no extra round trip — the function's own doc comment says exactly that (`:911-917`) | `postgres.go:918-936` |
| 3 | **`seq == 0` is the sentinel.** `pgx.ErrNoRows → return 0, nil`; "Real seqs start at 1, so 0 is unambiguous" | `postgres.go:927-928`, comment `:917` |
| 4 | **A dropped line does not even notify SSE** — `pg_notify` is skipped, "so SSE clients stay consistent with what readers can see" | `postgres.go:932-934` |
| 5 | **Single-line path: `seq == 0` ⇒ `slog.Warn("dropping log line for sealed run", "run", …)` then `204`.** The rationale is in the code: "204 keeps unmodified agents from retry-storming — same philosophy as FinishRun's alreadyFinalized response" | `api_agent.go:564-570`, 204 at `:570` |
| 6 | **Bulk path: per-line `seq == 0` increments `dropped` and overwrites `droppedRun`; one aggregate `slog.Warn` after the loop; then `204`** | `api_agent.go:719-738`; counter `:730-733`, warn `:736`, 204 `:738` |
| 7 | **The bulk warning logs only the LAST run id seen** (`droppedRun = req.RunID`, unconditionally, every drop), so in a batch spanning runs the earlier runs' drops are counted into `dropped` and their ids never appear | `api_agent.go:732` |
| 8 | **A mixed-run batch is constructible through the documented route.** The handler reads `req.RunID` **from each body element**, never from the `{runId}` path parameter of `POST /api/v1/agents/{agentId}/runs/{runId}/steps/{stepIndex}/logs/bulk`; only `{agentId}` is read from the path. Every distinct `RunID` is guarded first, in its own pass | `api_agent.go:697-718` (`chi.URLParam(r, "agentId")` at `:703`); route at `server.go:255` |
| 9 | **A 204 is not an error to the agent client**, so the batch is never queued in `p.pending` and is never retried. `Client.do` only errors on `>= 400`; `AppendLogBulk` returns that error verbatim; `flushLocked` appends to `pending` **only** on a non-nil error, and the buffer was already `Reset()` before the send | `client.go:107-113`, `:300-304`; `runner.go:369-392` (`p.buf.Reset()` `:371`, `appendPendingLocked` only under `if err != nil` `:390-392`) |
| 10 | **So the drop marker can never fire for this channel.** `droppedLines` is incremented **only** by `appendPendingLocked`'s cap eviction (`runner.go:432`), which requires the batch to have been queued, which requires an error, which a 204 is not | `runner.go:401-424`, `:429-435` |
| 11 | **Only `CreateLogArchive` writes the seal, and its only caller is `archiveRunLogs`** — the archiver, and nothing else, seals | `postgres.go:1519-1528`; `archiver.go:106`; `grep -rn "CreateLogArchive" --include=*.go` |
| 12 | **A run becomes sealable the instant it is terminal.** `ListRunsNeedingArchival` selects `status IN ('Succeeded','Failed','Cancelled') AND id NOT IN (SELECT run_id FROM run_log_archives)`, oldest-first, limit 20 | `postgres.go:1458-1466` |
| 13 | **The archiver's interval is hardcoded at the call site** — `30*time.Second`, no flag, no env. The parameter exists (`archiver.go:19-22`) but nothing sets it otherwise. Widening the window needs a code change | `cmd/controller/main.go:400` |
| 14 | **Archival is leader-elected** (`logArchiverLockKey = 0x6C6F6761`), so exactly one replica seals, and a failing candidate is excluded by a per-process backoff (1 min → 1 h) | `archiver.go:15`, `:28`, `:39-52` |
| 15 | **`line_count` / `max_seq` record what the archive object covers**, and `TrimRunLogs` refuses to trim a run that grew after archival (`ErrArchiveIncomplete`) — **which is the reason the seal exists at all**, per the `AppendLog` doc comment | `archiver.go:99-106`; `postgres.go:911-917` |
| 16 | **All three F2 agent endpoints pass `rejectTerminal=false`** — logs single, logs bulk, sidecar status. The terminal gate is NOT what stops a late log line | `api_agent.go:551`, `:709`, `:760` (Correction 1) |
| 17 | **`CloseScopes` is deferred at `orchestrator.go:209` and therefore runs after `FinishRun` at `:787-788`** — the plan's structural window is real in the code and unreachable on this rig (§Vehicle) | `orchestrator.go:209`, `:787-788`; `backend_host.go:362-368` |
| 18 | **The agent destroys every controller error body** (`&HTTPError{… Body: "response omitted"}`), so nothing controller-side is diagnosable from a run's own logs. Capture the controller's `slog` container-side, and **before** any `up -d --force-recreate` | `client.go:107-108` |

**The single sentence the scenario tests:** the controller accepts a log write,
tells the sender it succeeded, throws the data away, and records that fact in
exactly one place the sender can never see.

---

## Stack

```bash
cd test/ha
export MSYS_NO_PATHCONV=1          # Git Bash rewrites container paths (W2-5)
export COMPOSE_FILES="-f docker-compose.ha.yaml"
docker compose $COMPOSE_FILES up -d --build
```

- **Plain `test/ha`, no overlay.** `bigbody` is not needed (no large bodies),
  `logfault`/`steplink` are not needed (no per-URI fault), and **the three nginx
  overlays do not stack** anyway. `s3proxy` is deliberately absent: this
  scenario needs Garage to *work* so the archiver can seal.
- **Garage is required.** Without an object store the archiver never runs, no
  run is ever sealed, and every arm is a silent no-op (`G1` gates this).
- **`down -v`, not `down`.** `ha-garagedata` is persistent; a stale archive
  object from a previous attempt would make a "sealed" claim unfalsifiable.
- **Part B uses `inject.sh partition agent1` / `heal agent1`**, a full
  docker-network disconnect. Deliberately **not** `nginx-block`: that arm needs
  an `nginx -s reload` to take effect on an already-connected agent, which W2-5
  has a captured counter-example for. A network disconnect has no reload in it.
  **Verify the agent container is on `unified-cd-ha_default`** first or
  `partition` silently no-ops (`inject.sh:68`).

Throughout: `psql` means
`docker compose $COMPOSE_FILES exec -T postgres psql -U unified -tAc "<sql>"`,
`API` means `curl -sS -H "Authorization: Bearer ha-admin-token"` against
`http://localhost:18080`, and `mc` means
`docker compose $COMPOSE_FILES exec -T mc mc`, which talks to `garage:3900`
directly and is the out-of-band surface for every object claim.

**Workloads.** `test/edgecase/workloads/w35-probe.payload.json` (job
`edge-w35-probe`, `agentSelector: [kind:w35probe]`) — a job **no real agent can
claim**, so the synthetic identity is the only possible claimant and agent1 /
agent2 are never involved in Parts A and C. `longrun.payload.json`
(`edge-longrun`, 300 × `tick N` at 1/s) for Part B. `tick.payload.json`
(`edge-tick`) for the G6 archiver positive control.

**A Windows/MSYS trap that has already cost this wave a capture.** With
`MSYS_NO_PATHCONV=1` set, mingw `curl` cannot open `@/c/Users/...` and **uploads
an empty body while still returning 204**. Use Windows-form paths
(`C:/Users/...`) for every `curl` file argument and check `%{size_upload}`,
never the status alone.

---

## BASELINE GATE — do not proceed past a failing check

Write every gate output to `$SCRATCH/gate.txt` unless a per-check file is named.

```bash
SCRATCH="<scratchpad>/w3-5" ; mkdir -p "$SCRATCH"
```

1. **G0 — worktree.** `git rev-parse --show-toplevel` is `.../wt-edge-spec`,
   branch `plan/edge-case-w3`. `docker compose ls` shows the developer stack
   (project `unified-cd`) present and untouched. **STOP** if the toplevel is the
   main checkout.
2. **G1 — Garage is there and the controllers are using it.** All three
   controllers log `using S3-compatible object store`; **none** logs
   `no object store configured`. **STOP on any mismatch** — with no object store
   nothing is ever sealed and every arm below silently measures nothing.
   → `$SCRATCH/gate-g1-objstore.txt`.
3. **G2 — stack health.** Three controllers up, `API /readyz` → 200,
   `GET /api/v1/agents` lists agent1 and agent2 with `kind:linux`.
4. **G3 — clean slate, object side included.**
   `SELECT count(*) FROM runs;` and `SELECT count(*) FROM run_log_archives;` → 0;
   `mc ls --recursive garage/unified-cd-logs/` → **empty**. **If it is not empty
   the previous teardown was not `-v`.** → `$SCRATCH/gate-g3-clean.txt`.
5. **G4 — the archiver's positive control, and the seal latency measured not
   assumed.** Trigger `edge-tick`; record `Succeeded` instant; poll
   `run_log_archives` until the row appears; record the delta. Confirm the
   object `runs/<id>/logs.ndjson` exists via `mc` and that `line_count` equals
   the `logs` row count. **STOP if no row appears within 90 s** — Parts A and B
   both time against this. → `$SCRATCH/gate-g4-archive.txt`.
6. **G5 — retention and trim are OFF.** All three controllers log
   `run retention disabled (keep forever)`; `UNIFIED_LOG_TRIM_DAYS` unset. A live
   sweeper would delete or trim runs out from under the measurements.
   → `$SCRATCH/gate-g5-retention.txt`.
7. **G6 — the synthetic agent works end to end, before anything is sealed.**
   Enroll → exchange → post job → trigger → claim → push one `logs/bulk` batch →
   confirm the lines are **in `logs`** → `finish` Succeeded. **STOP if a
   pre-seal bulk push does not land**, because then Part A's post-seal zero
   proves nothing. → `$SCRATCH/gate-g6-synthagent.txt`.
8. **G7 — API 500s.** The API on this rig has been intermittently returning 500s.
   Record, for every gate and every request, how many attempts it took. A 500 on
   a trigger is **not** a finding of this scenario; a 500 mid-measurement
   invalidates that attempt, which is discarded and re-run, not reasoned around.

---

## Part A — the real seal, the synthetic sender

**Deliverable:** a **`Succeeded`** run, sealed by the real archiver, whose log
is permanently short by exactly `M` lines that the controller accepted with
`204`, with the count reconciled from four independent surfaces.

**A1 — build the synthetic agent.** Record every response.
→ `$SCRATCH/partA-synthagent.txt`.

1. `API -X POST /api/v1/agent-enrollments -d '{"agentId":"w35-probe","labels":["kind:w35probe"],"expiresIn":"2h"}'`
   → **201**, capture `.token`.
2. `curl -X POST -H "Authorization: Bearer <enrollment token>" /api/v1/agents/enroll`
   → **200**, capture `.accessToken` (`uca_…`, TTL 1 h,
   `api_agent_enrollment.go:21`). Note the expiry in the capture.
3. `API -X POST /api/v1/jobs` with `w35-probe.payload.json`, then
   `API -X POST /api/v1/runs -d '{"jobName":"edge-w35-probe"}'` → run id.
4. `curl -H "Authorization: Bearer <access>" -X POST "/api/v1/agents/w35-probe/claim?timeout=10s"`
   → the run, now `Running`, `claimed_by = w35-probe` (the claim upserts the
   agent registration itself, so no separate `/register` call is needed).

**A2 — pre-seal baseline, which is what makes the post-seal zero mean
something.** Push `N = 20` lines via
`POST /api/v1/agents/w35-probe/runs/<id>/steps/0/logs/bulk` with a body of `N`
`LogAppendRequest` objects (`runId`, `stepIndex`, `stream`, `timestamp`, `line`
— `api/types.go:213-219`), lines self-indexing (`pre-<i>`). Expect **204**.
Confirm `SELECT count(*) FROM logs WHERE run_id=…` → **N**.
→ `$SCRATCH/partA-preseal.txt`.

**A3 — finish the run and let the REAL archiver seal it.**
1. `POST /api/v1/agents/w35-probe/runs/<id>/finish -d '{"status":"Succeeded"}'`
   → 204. Stamp the instant. Confirm `GET /api/v1/runs/<id>` is `Succeeded`.
2. Poll `SELECT archived_at, line_count, max_seq FROM run_log_archives WHERE run_id=…`
   every 2 s until the row appears. **Stamp the instant it appears and record
   the delta from the finish.** Confirm `mc ls runs/<id>/logs.ndjson` exists.
   → `$SCRATCH/partA-seal.txt`.
3. **This is the load-bearing sentence of the whole scenario and the capture must
   support it: the seal was written by the archiver, not by this runbook.**
   Capture the controller's `archived Run logs` `slog.Info`
   (`archiver.go:117`) for this run id, container-side, and name which replica
   emitted it.

**A4 — the post-seal push.** Push `M = 10` more lines (`post-<i>`) through the
**same** bulk route, stamping before and after.
1. Expect **204**. Record the status verbatim and the body (empty).
2. `SELECT count(*) FROM logs WHERE run_id=…` → still **N**.
3. `SELECT line FROM logs WHERE run_id=… AND line LIKE 'post-%'` → **0 rows**.
4. Controller logs, container-side, filtered to the window: exactly one
   `dropping log lines for sealed run` with `run=<id>` and `dropped=M`.
   **Name the replica** — it need not be the archiving one.
5. `GET /api/v1/runs/<id>/logs/stats` → `count` = N.
6. `run_log_archives.line_count` = N, unchanged; `archived_at` unchanged.
→ `$SCRATCH/partA-postseal.txt`.

**A5 — the single-line path, separately.** Push one line through
`POST /api/v1/agents/w35-probe/logs` (body: one `LogAppendRequest`). Expect
**204** and exactly one `dropping log line for sealed run` (singular).
**Both paths must be exercised** — the plan cites two different sites and an
entry that tests one and claims both is the failure mode this wave keeps
paying for. → `$SCRATCH/partA-single.txt`.

**A6 — the sender's blindness, which is the finding's core.** From A4/A5's
captures state, each with its source:
- the sender received `204` and nothing else (**the response body is empty**);
- there is **no** `[N log line(s) dropped: controller unreachable]` line in the
  run (`SELECT … WHERE line LIKE '%dropped%'` → 0), and **fact 10 explains why
  there never could be**;
- the run is `Succeeded`, the archive is `readable` and internally consistent,
  and **nothing in the DB records that anything was lost**.

**A7 — label the instrument, every time it carries a claim.** What is synthetic
is *who* holds the credential. The seal, the handler, the guard, the `INSERT`,
the 204 and the warning are all the product's. The production shape this stands
in for is a sidecar's post-`CloseScopes` flush (fact 17) — **which this rig
cannot run** — or an agent retrying after a partition, which **Part B attempts
for real**.

**Falsification.** If the post-seal lines **land** (count goes to N+M), fact 1
or 11 is wrong and that is the finding — report it as a correction to the plan
rather than burying it. If the push returns anything other than 204, record it:
a 4xx would mean the agent *does* learn, which would substantially weaken the
entry and must be reported as such.

---

## Part B — the natural race: a real agent, a real seal, one injected partition

**Deliverable:** a run whose lines were buffered by a **real agent** during a
partition and re-offered after heal, by which time the archiver had sealed the
run — with the controller warning naming that run, the agent's own log showing
no error for the push, and the run's `logs` permanently short.

**This is the only arm in which nothing about the sender is synthetic**, and it
is the shape `docs/troubleshooting.md:865-866` names as a common cause. It is
also the only arm that can fail. **Cap: 3 attempts. Report the count either
way.**

**Why this produces the window when nothing else on this rig can.** A real
agent's log pusher only re-offers `p.pending` while the pusher is alive, i.e.
while the step is still running. Partitioning the agent keeps the step running
(the cancel poller's requests fail and are simply retried,
`orchestrator.go:122-147`) **and** makes every `AppendLogBulk` fail, so batches
accumulate in `p.pending` (`runner.go:390-392`). Cancelling the run marks the
row terminal **synchronously and controller-side**
(`MarkRunFinished(..., api.RunCancelled)`, `api_runs.go:374`) without the agent
knowing. The archiver then seals it on its next tick. Heal, and the pusher's 2 s
auto-flush re-offers the whole backlog into a sealed run.

**B1 — arm.** Confirm `agent1` is on `unified-cd-ha_default`
(`docker inspect`), record its IP. → `$SCRATCH/partB-arm.txt`.

**B2 — the sequence**, every step stamped `date -u +%FT%T.%3NZ`:
1. Trigger `edge-longrun`. Wait until `Running` **and** until `logs` shows ≥ 10
   lines, so the pusher is demonstrably working. Record which agent claimed it —
   **if it is not `agent1`, partition whichever agent did**, or retrigger.
2. `inject.sh partition <agent>`. **Probe-confirm the arm**: the run's `logs`
   count must stop advancing (sample it three times over ~10 s and record all
   three). An arm that did not take is the W2-5 trap and invalidates the
   attempt.
3. `API -X POST /api/v1/runs/<id>/cancel` (`server.go:374`) → expect 204.
   Stamp. Confirm `SELECT status FROM runs WHERE id=…` is `Cancelled`
   **immediately** — `handleCancelRun` calls
   `MarkRunFinished(r.Context(), id, api.RunCancelled)` synchronously
   (`api_runs.go:374`), which is the whole point of using cancel here.
4. Wait for the seal: poll `run_log_archives` until the row appears; stamp it
   and record the delta from the cancel. **Record `line_count`** — this is the
   count the archive froze at, and everything after is lost.
5. Wait a further ≥ 30 s so the partition is comfortably longer than the seal,
   then `inject.sh heal <agent>`. Stamp.
6. Watch the controller logs for `dropping log lines for sealed run` with this
   run id. Record the instant, the `dropped` count, and the replica.
   → `$SCRATCH/partB-race.txt`.

**B3 — the four claims, each from a named capture.**
1. `SELECT count(*) FROM logs WHERE run_id=…` equals `run_log_archives.line_count`
   — the run's log is frozen at the seal.
2. The controller warned with `dropped=<K>` for this run id, **after** the heal
   instant and **after** the seal instant.
3. The agent's own container log over the same window contains **no** error for
   the log push. Grep it and paste the count. (It will contain plenty of errors
   from *during* the partition — those are expected and are not this claim.
   Scope the grep to the post-heal window and say so.)
4. There is **no** `[N log line(s) dropped: controller unreachable]` line in the
   run's `logs`.

**B4 — separate the two loss channels, line by line.** The run emits
`tick 1 … tick 300`, self-indexing, so the surviving set is exactly enumerable.
Compute:
- `L_seal` = ticks emitted **after** the seal instant that the controller
  `204`'d away → **this scenario's loss**;
- `L_flush` = ticks the agent never delivered at all because the step-end
  `Flush` budget expired → **W1-2/W1-6's loss, cited not re-measured**.

The discriminator is the controller's `dropped=<K>`: lines counted there
reached the controller. **If the two cannot be cleanly separated from the
captures, say so and report only `K`** — an unseparated total is exactly the
kind of number this wave has had to withdraw.

**B5 — if the race is not hit in 3 attempts.** File the code-read argument
(facts 9, 10, 12, 13) with an explicit **"not reproduced live"** label — the
W2-3 Arm D precedent, accepted at 0/10 — and **do not write that the window is
unreachable**. Part A stands on its own.

**Falsification.** If after heal the agent's backlog **lands** in `logs`, the
run was not sealed when the backlog arrived: check the seal instant against the
warning instant before concluding anything. If the pusher is dead by heal time
(the step ended), the attempt is void, not a result — shorten the partition or
lengthen the workload.

---

## Part C — the mixed-batch attribution gap

**Deliverable:** one bulk request spanning **two** sealed runs in which
`dropped` counts both and the warning names only one.

**C1 — two sealed synthetic runs.** Repeat A1-A3 for a second `edge-w35-probe`
run under the **same** `w35-probe` identity (ownership is per-agent, and the
handler guards each distinct `RunID` in its own pass, fact 8 — a batch spanning
two runs the same agent owns passes the guard). Both must be `Succeeded` and
both must have a `run_log_archives` row before C2.
→ `$SCRATCH/partC-setup.txt`.

**C2 — the mixed batch.** POST **one** body to
`/api/v1/agents/w35-probe/runs/<idA>/steps/0/logs/bulk` containing, in order:
2 lines with `runId = <idA>`, then 2 lines with `runId = <idB>`. Expect **204**.
→ `$SCRATCH/partC-mixed.txt`.

**C3 — the claims.**
1. Exactly **one** `dropping log lines for sealed run` line, with
   `dropped=4` and `run=<idB>` — the **last** id in the body, not the path's.
2. `<idA>` appears **nowhere** in the controller's warning output for that
   request. Grep for it and paste the zero.
3. Neither run's `logs` count changed.
4. **Note what this also proves incidentally, since the capture is free:** the
   `{runId}` path parameter is decorative for this handler — the body governed
   (fact 8). Say whether that is itself worth recording.

**C4 — order-sensitivity, one extra request, because it makes the gap
concrete.** Send the same four lines with the order **reversed** (`<idB>` first,
`<idA>` last) and confirm the warning now names `<idA>`. **The same physical
loss is attributed to a different run depending on batch order** — that is the
sentence the entry should end on. → `$SCRATCH/partC-order.txt`.

**C5 — label the instrument.** A real agent's `LogPusher` is per-run and
per-step (`runner.go:233-235`) and calls `AppendLogBulk` with a single `runID`
(`client.go:300-304`), so **a real agent never sends a mixed batch today**.
Part C is therefore a demonstration that the **handler** mis-attributes a shape
the **documented route** accepts, not a claim that agents produce it. **Say
exactly that**, and state the consequence honestly: the gap is latent, reachable
by any client of the documented API, and would become live the moment a pusher
is ever made to coalesce runs. **This bounds the severity and the entry must
let it.**

---

## Part D — the codebase's own accommodation, and what it did not extend

**Code-read only. No execution.** Read Correction 1 before writing a word of
this: the brief's framing ("sidecar status got an accommodation, sidecar logs
got none") is **wrong at HEAD** and must not be repeated.

What is actually there:

- `handleAgentSidecarStatus` carries a comment (`api_agent.go:741-752`) that
  **states the post-`FinishRun` ordering explicitly** — "both agents stop their
  sidecar pumps via a deferred CloseScopes that runs AFTER FinishRun, so the
  final reportStatus(..., \"exited\", exitCode) call is *expected* to arrive once
  the run is already terminal" — and justifies letting it through: the write is
  "a display-only upsert keyed by (run, index), so a late/duplicate write here
  is harmless."
- **The same comment's reasoning applies verbatim to that sidecar's final log
  lines**, which leave on the same `CloseScopes` (`sidecar_logs.go` flush on a
  `context.WithoutCancel`) — and those pass the terminal gate too (fact 16) but
  hit the **seal** ~30 s later.
- **So the project has documented the window, reasoned about what goes through
  it, and closed it for the display-only half while the data half is discarded
  with a 204.** That is the entry's sharpest sentence and it is available
  without running anything.

**Do not overclaim it.** The seal has a real justification (fact 15: storing
late lines makes the run untrimmable and, after trim, invisible), so "just
accept them" is not a fix. The defensible recommendations are about
**visibility**, not about accepting the write: return a body or a distinguishing
status the sender can act on; or write a synthetic marker into the run's own log
(the archiver could append to the *archive*, which it owns); or at minimum
attribute the bulk warning per run (Part C). **State the counter-argument to
each.**

---

## Part E — the contract survey

Run §"Contract survey" in full, capture untruncated with hit counts, and run the
already-ruled check. → `$SCRATCH/partE-docs.txt`. Report the counts in the entry
even when the answer is "documented and sanctioned" — a survey whose count is
not reported is the W3-3 failure.

---

## Teardown

```bash
docker compose $COMPOSE_FILES ps
docker compose $COMPOSE_FILES exec -T mc mc ls --recursive garage/unified-cd-logs/
docker compose $COMPOSE_FILES down -v
```

- **Heal any partition first** and confirm it (`docker network inspect`), or the
  next scenario inherits a disconnected agent.
- **Cancel every surviving run** and confirm zero non-terminal runs in a census.
  The synthetic identity's runs count.
- **Revoke the synthetic agent's credential** (`DELETE /api/v1/agents/w35-probe`)
  or record that the `down -v` disposes of it. Asserting neither is not an
  option.
- **Kill every background sampler and *capture* that, do not assert it.** Keep
  PIDs in `$SCRATCH/samplers.pid`, `kill` them, show `jobs` empty and
  `ps -W | grep -iE "curl|psql|mc"` matching nothing — **and check inside the
  containers too**, because a `docker compose exec` sampler outlives the shell
  that launched it and appears in neither (the W3-4 lesson).
  → `$SCRATCH/teardown.txt`.
- **`down -v`, not `down`** — `ha-garagedata` is persistent and this scenario's
  claims are all about what is and is not in it.
- Copy `$SCRATCH` into the campaign evidence root at the wave checkpoint.

---

## Recording rules

- **Lead every entry with the doc sanction** (`docs/operations.md:51`,
  `docs/troubleshooting.md:851-868`) and then argue what the docs do not say.
  An entry that presents the drop as undocumented is wrong and will be caught.
- **Judge against I4 clause by clause, quoted verbatim**, and say which clauses
  are *not* touched. Do not stretch "archives stay readable".
- **Every arm names its instrument every time it carries a claim** — Part A's
  synthetic sender, Part C's hand-built mixed body. Part B, if it lands, names
  none, and that difference is worth stating.
- **Say plainly, in the runbook and in every entry, that no arm reproduces the
  plan's structural sidecar race**, and why (§Vehicle). The entries are
  demonstrations of a code path plus, if Part B lands, one natural race by a
  different route.
- **Separate this scenario's loss from W1's by citation, never by
  re-measurement.**
- **Observation entries say "observation" in the title** and repeat it in the
  Severity line as `minor (observation)` (`FINDINGS.md:481`).
- **Every number cites a `$SCRATCH` filename whose time window covers it.**
  Derived figures say "derived"; code-read figures say "code-read"; uncaptured
  live observations say `(observed live, raw output not captured to
  scratchpad)`. **Do not present verbatim-looking log text that was not
  captured**, and do not call an attribution a "bracket" unless a capture covers
  that specific request.
- **Report every capped arm's attempt count either way**, with the cap stated.
- **When claiming a class is fully enumerated, paste the enumeration.** W3-6
  claimed two producers and a third was sitting in its own capture.

---

## Execution notes — 2026-07-31 run (read before re-running)

Executed on branch `plan/edge-case-w3`, **`05:40:18Z – 05:53:02Z`**, on the single
stack §Stack specifies (plain `test/ha` + Garage + `mc`, no overlay, no
interposer), torn down with `down -v` (`w3-5/teardown.txt`). **Three `FINDINGS`
entries: 2 violations (1 major I4, 1 minor I7) and 1 observation (minor).** No
branch-internal asset bug. The developer stack (`docker compose ls`, project
`unified-cd`) was running and untouched at both ends.

**Four runs, all reaching exactly one terminal state** (`census.txt`): `edge-tick`
`3d32270a` Succeeded, `edge-w35-probe` `cd7c25a0` and `edb91160` Succeeded (both
synthetic), `edge-longrun` `c158dcd4` Cancelled. Zero non-terminal runs at
teardown. **I1 held.**

| Part | Result |
|---|---|
| G4 | archiver positive control: `edge-tick` terminal `05:41:27.653Z`, sealed `05:41:49.545Z` — **21.89 s**, `line_count=30` = `logs`=30, object 3038 B |
| A | **the whole point, and it worked first time**: 20 lines land pre-seal; run finished `Succeeded`; **the real archiver** sealed it 2.62 s later (`archived Run logs` on controller1); 10 post-seal lines → **204, zero-byte body**, `dropped=10` on controller3, `logs` still 20 |
| A5 | single-line route also 204 + `"dropping log line for sealed run"` — **and that warning carries no count field**, unlike the bulk form's `dropped` |
| B | **the natural race, HIT ON ATTEMPT 1 OF A CAP OF 3.** Real agent, real seal, one `partition`. **78 lines dropped, measured; 78 expected, derived — exact.** Post-heal agent log: **one INFO line, zero errors** |
| C | mixed batch of 4 across two sealed runs → `dropped=4` naming only the **last** id; reversed order names the **other** run. Same loss, different attribution |
| D | code-read only; produced two corrections to the plan/brief (below) |
| E | docs survey run in full, counts **35 / 49 / 84 / 1**; the 84 filtered to log/agent hits → **0** |

**Ten things a re-run should know.**

1. **THE PLAN'S PREMISE IS WRONG IN A SECOND WAY NOBODY HAD NOTICED.** Beyond
   sidecars being unavailable (Task 3), the sidecar race is **not structural**
   even where sidecars exist: `CloseScopes` follows `FinishRun` by
   microseconds-to-milliseconds (`orchestrator.go:209` vs `:787-788`) while the
   archiver can only seal on its next **30 s** tick after that same commit
   (`main.go:400`). **The flush wins essentially always.** Do not design a future
   sidecar-capable scenario around "every run".
2. **The realistic producer is delayed delivery, and it is cheap.**
   `partition agent → cancel → wait for the seal → heal` hits on the first
   attempt and takes ~95 s. It is also the exact cause
   `docs/troubleshooting.md:865` names. **Prefer it to any hand-seal.**
3. **NO ARM SEALS BY HAND.** `test/edgecase/README.md` advises
   `INSERT INTO run_log_archives`; that advice is now superseded — a synthetic
   *sender* against a real archiver seal is strictly stronger, and costs the same
   five API calls W3-6's instrument does.
4. **The seal latency is variable and must be measured, not assumed.** Observed
   **21.89 s**, **2.62 s** and **26.9 s** in one session, because a 30 s ticker
   is sampled at whatever phase the run finishes in. Poll for the row; never
   `sleep 30`.
5. **Probe-confirm the partition by watching the log count freeze.** Three
   samples at 12/12/12 over 9.2 s, and the last stored line's own `ts`
   (`05:47:07.066Z`) sits 177 ms before the partition instant. That is the
   cheapest possible arm confirmation and it is exact.
6. **The 2 s auto-flush cadence is visible in the stored rows** — two lines share
   each `ts` — which is what makes the 78/78 accounting checkable rather than
   asserted.
7. **Grep every replica.** Part B's nine drop requests were served by **all
   three** controllers within a 3 s burst, and the replica that sealed the run
   (controller1) is not the one that warned about Part A's bulk drop
   (controller3). A single-container grep undercounts.
8. **The MSYS trap fires here too, but LOUDLY.** `curl -D/-o` with MSYS-form
   output paths gives exit 23 / `http_code=000` / `size_upload=0` — the request
   never leaves the client. (Contrast W3-6 note 1, where an MSYS `@` path
   uploaded an empty body and still returned 204.) Windows-form paths for every
   `curl` file argument, and check `%{size_upload}`.
9. **`DELETE /api/v1/agents/{id}` is an AGENT-auth route** (`server.go:246` has
   no `agentRouteOrServerAuth`), so the admin token gets **401**. Deregister the
   synthetic identity with its own credential — it returns 204.
10. **The rig's intermittent-500 allowance (G8) was never spent** — nothing was
    retried for a 500. **That is an absence of trouble, not a measurement**: no
    capture counts 500s across the session, and no `FINDINGS` entry cites it.

**Sampler hygiene was captured, not asserted.** **No background job was launched
by any driver** — every `curl`/`psql`/`mc` call ran in the foreground and was
waited on — so `$SCRATCH/samplers.pid` is deliberately empty, and the capture
proves that rather than claiming it: `jobs` empty, host `ps -W` for
`curl|psql|mc` matching nothing, and an in-container `ps` on `postgres`, `nginx`
and `controller1` showing nothing left (the W3-4 lesson). **The `mc` image has no
`ps`**, which `teardown.txt` records rather than hides.

**No Postgres statement logging was armed**, so there was nothing to revert.

**One runbook step was not followed as written.** Part B's step 2 says "if the
claiming agent is not `agent1`, partition whichever agent did" — `agent2`
claimed, and `agent2` was partitioned. Recorded because the runbook names
`agent1` throughout and a reader diffing the captures will see `agent2`.
