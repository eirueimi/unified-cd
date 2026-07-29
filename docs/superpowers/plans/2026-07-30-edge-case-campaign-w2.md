# Edge-Case Campaign: Wave W2 (Reaper / Timing Boundaries) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Execute Wave W2 (reaper / timing-boundary scenarios) of the edge-case campaign: nine live scenarios against the `test/ha` compose stack, recording findings.

**Architecture:** Same pattern as W0/W1 — per-scenario runbooks under `test/edgecase/scenarios/`, findings appended to `test/edgecase/FINDINGS.md`, raw captures written to the session scratchpad during the run and copied to the out-of-tree evidence root at the wave checkpoint.

**Tech Stack:** docker compose (`test/ha` + overlays), curl against LB `localhost:18080` (token `ha-admin-token`), psql for lock/queue inspection, `test/edgecase/tools/inject.sh`.

## Global Constraints

- All committed text is English (AGENTS.md).
- Work on branch `plan/edge-case-w2` in worktree `wt-edge-spec` — never commit on the main checkout.
- **No production-code changes** (spec §8). Test-only files under `test/edgecase/` and docs. `test/ha/` is NOT modified — use overlays.
- Findings record problems; they do not fix them. Classification: **violation** = observed behavior contradicts an invariant (I1-I7) or a documented contract; **observation** = behavior matched expectations but reveals a risk. An unexported helper's own doc comment does NOT count as the documented contract.
- A third bucket exists per the W1 checkpoint: a defect in the campaign's **own** assets, fixed inside the branch, is reported outside both tallies.
- Scenario tasks follow: write runbook → commit → execute → record findings → commit findings separately.
- Every number in FINDINGS.md must trace to a capture whose time window covers the claim. Derived or extrapolated figures must say so. Uncaptured live observations must be annotated `(observed live, raw output not captured to scratchpad)`.
- Tear every stack down with `-v` after each scenario. Bash timeouts up to 600000 ms.

## Verified code facts (do not re-derive)

**These were read from source at `b3c278e`. Per the W1 methodological lesson, they are still claims — if execution contradicts one, that contradiction is itself a finding; do not silently work around it.**

- **Line numbers shifted by PR #113 (`2c018fe`, +22 lines in `postgres.go` below :807).** `ListStuckRunIDs` is at `internal/store/postgres.go:1238` (not :1216); `DecideApproval` at `:2439` (not :2421). The W1-5 runbook cites the old numbers and is stale in that respect.
- **Eleven leader-elected background jobs**, all session-level `pg_try_advisory_lock` via `AcquireAdvisoryLock` (`postgres.go:1425-1450`), wired in `cmd/controller/main.go:367-449`:

  | Job | Lock key | Interval | Wired |
  |---|---|---|---|
  | Scheduler | `0x65786364` | 200ms | main.go:367 |
  | Log archiver | `0x6C6F6761` | 30s | :400 (only if `obj != nil`) |
  | Cache cleanup | `0x63616368` | 24h | :401 (only if `obj != nil`) |
  | Approval reaper | `0x61707276` | 1m | :403 |
  | Stuck-run reaper | `0x7374756B` | 30s | :404 |
  | Queued-run reaper | `0x71756575` | 30s | :410 |
  | AppSource sync reaper | `0x73796E63` | 30s | :414 |
  | Audit retention | `0x61756474` | 1h | :420 (no-op unless `--audit-retention-days > 0`) |
  | Run retention | `0x7272746E` | 1h | :426 (no-op unless configured) |
  | Log trim | `0x6C74726D` | 1h | :436 (no-op unless `obj != nil && trimDays > 0`) |
  | AppSource reconciler | `0x61707073` | 30s | :446-449 |

- **Three background jobs have NO leader election** and run on every replica: `RunGitResolver` (`main.go:437-445`, 200ms tick, `resolveGitPendingRuns` at `scheduler.go:290` acquires no lock — and its `failureBackoff` comment at `scheduler.go:271-274` presumes a leader it does not elect); the inline `DeleteStaleAgents` loop (`main.go:382-398`, 1m tick, 5m threshold); the inline `DeleteExpiredOIDCStates` loop (`main.go:368-381`, 10m tick).
- **Only the scheduler holds its lock across ticks** (`scheduler.go:30,45-56`); the other ten acquire and release per tick. `"scheduler became leader"` (`scheduler.go:55`) is logged once per acquisition and is the only leadership log line in the system.
- **Ten of eleven jobs are silent both when not leader and when idle** (`if release == nil { return }` with no log; all summary lines guarded by `if n > 0`). Leader exclusion therefore cannot be verified from logs in the idle case — use `pg_locks` (`SELECT objid, pid FROM pg_locks WHERE locktype='advisory'`; all keys fit in 32 bits so `objid` maps 1:1 to the table above) or make the job produce work.
- **Stuck-run reaper:** `RunStuckRunReaper(ctx, st, 30s, 90s, 60s)` at `main.go:404`. `ListStuckRunIDs` (`postgres.go:1238-1248`) binds `$1`=staleAfter(90s), `$2`=grace(60s): `claimed_at < NOW()-60s AND (a.id IS NULL OR a.last_seen_at < NOW()-90s)`.
- **`failOrphanedRun` is now three sequential non-transactional writes** (`stuckrun_reaper.go:76-90`): `MarkRunStepsInterrupted` (:81, error returned — a failure here blocks the run from being failed at all), `MarkRunFinished` (:84), `cancelDescendantRuns` (:88). `ListStuckRunIDs` only selects `status='Running'`, so a run failed at :84 is never re-listed and a crash before :88 is never re-driven.
- **`DeleteStaleAgents`: 1m tick, 5m threshold, no lock** (`main.go:382-398`, `postgres.go:1225-1233`). Once it removes the agent row the reaper's `a.id IS NULL` branch matches **unconditionally** — the only remaining gate is `claimed_at < NOW()-60s`, so a run claimed by a deleted agent is reapable indefinitely. The two windows are strictly nested (90s < 300s).
- **Queued-run reaper exists and is separate:** `RunQueuedRunReaper(ctx, st, 30s, queuedRunGraceDefault(), 90s)` at `main.go:410`. **`queuedRunGraceDefault()` reads env `UNIFIED_QUEUED_RUN_GRACE` (Go duration), default 5m** (`main.go:102-114`) — this is the lever for compressing W2-4. Predicate `ListUnclaimableQueuedRuns` (`postgres.go:1275-1286`) uses `created_at` (not a queued-at stamp) and is **label-only by design** — the 11-line comment at `postgres.go:1264-1274` ends "Do not add a capability clause to this query", so a capability-unschedulable run is left Queued forever on purpose and will NOT trip this reaper.
- **Three distinct orphan definitions — do not conflate:** startup reconcile `claimed_by=$1 AND status='Running'` with **no grace** (`postgres.go:269-285`); heartbeat reconcile adds `claimed_at < NOW()-60s` and subtracts the reported set (`postgres.go:292-294`, `api_agent.go:101-119`, `heartbeatReconcileGrace = 60s` at `:71`); stuck-run reaper as above.
- **`ReconcileRuns` sends no run list** (`internal/agent/client.go:142-146`) — the controller derives the set from `claimed_by`. `activeRunIds` is heartbeat-only (`api/types.go:72-81`).
- **`cancelDescendantRuns` is transitive** (BFS with `visited`, `api_runs.go:390-412`), marks children `Cancelled`, logs and continues on error, returns nothing. Called from five sites: stuck-run reaper (`:58`), heartbeat reconcile (`api_agent.go:114`), startup reconcile (`:824`), queued-run reaper (`queuedrun_reaper.go:77`), claim-build failure (`api_agent.go:188`).
- **`call:` children have no parent column.** `handleAgentCreateChildRun` (`api_agent.go:780-806`) uses `parentRunID` only for the ownership guard (:783) and calls `createRunFromJob(..., "agent:"+agentID)` (:800) without it. The sole persisted edge is `step_reports.child_run_id`, written by a **separate HTTP request** whose error is discarded: `callstep.go:50` creates the child, `:62-66` sends the link with `_ = client.ReportStep(...)`. `ListChildRunIDs` (`postgres.go:313-328`) is `SELECT child_run_id FROM step_reports WHERE run_id=$1 AND child_run_id IS NOT NULL`. `UpsertStepReport` uses `COALESCE(EXCLUDED.child_run_id, step_reports.child_run_id)` (`postgres.go:803`) so a link is never erased — the failure mode is never-written.
- **Scheduler fire is two transactions** (`scheduler.go:189-196`): `CreateRun`, then `UpdateScheduleLastFiredAt` — separate pool connections, no `Begin`/`Commit` anywhere in `checkAndFireSchedules` (:86-204). A failure of the second is logged at Warn and **not retried** (:195). `checkAndFireSchedules` runs on a 1-minute cadence inside the 200ms loop (:70-74); it fires when `next ∈ [now-1h, now]` and, when `next < now-1h`, **silently advances `last_fired_at` with no run and no log** (:197-201). Nothing checks whether a run already exists for an occurrence (`triggeredBy` is `"schedule:"+name`, no timestamp).
- **Nothing prevents two processes sharing one agent ID.** No fencing token, session, or generation column (`migrations/001_init.up.sql:18-27`); `UpsertAgent` is `ON CONFLICT (id) DO UPDATE` (`postgres.go:1083-1108`); the ownership guard is a bare string compare `run.ClaimedBy != agentID` (`agent_guard.go:121`). Heartbeat interval is 15s (`internal/agent/heartbeat.go:10`).
- **`DecideApproval` (`postgres.go:2439-2450`) guards only `WHERE run_id=$1 AND step_index=$2 AND status='Pending'`** — no join to `runs`, no run-status check, no `timeout_at` check. `handleDecideApproval` (`api_approvals.go:13-54`) never loads the run and returns **204** on `changed`. `MarkExpiredApprovalsTimedOut` (`postgres.go:2473-2484`) also guards only on `status='Pending' AND timeout_at < now()` — two unsynchronised writers, both blind to the run.
- **Two independent approval clocks.** Controller: `timeout_at = time.Now().Add(...)` at `api_approvals.go:86-89`, reaped on a **1-minute** tick (`main.go:403`) so a row can sit Pending up to ~60s past `timeout_at`. Agent: `deadline := time.Now().Add(...)` computed locally at `internal/agent/approval.go:48`, polled every `ApprovalPollInterval` (`agent.go:23`); on expiry it logs `"approval timed out"` and returns `false`, failing the step — and per `approval.go:17-20` **the controller-side row stays `Pending`** because the agent has no decision endpoint. **The agent-local timeout is the primary producer of the vulnerable window; it does not require the reaper to be late.**
- **Audit rows are written synchronously after the response** (`audit.go:172-232`, applied at `server.go:357`). Action `run.approval.decide` (`audit.go:27`), resource = the runID URL param (`audit.go:63`), and **`rec.Status()` is recorded** — so the buggy path leaves `action=run.approval.decide, resource=<Failed run id>, status=204`, distinguishable from 409 (already decided) and 404 (no row). Readable via `GET /api/v1/audit` (admin-only, `server.go:363`). The approval reaper writes no audit row; a timed-out row shows `decided_by='system'`.
- **W2-9's original premise was inverted.** Claiming is two-phase. Phase 1 `TransitionPendingToQueued` (`postgres.go:437-475`) selects `WHERE status='Pending' ORDER BY created_at LIMIT $1` with **limit 50** from `scheduler.go:58`, then calls `tryQueueRun` per candidate. Phase 2 `claimNextRun` (`postgres.go:679-724`) runs over **`Queued`** rows with `ORDER BY created_at LIMIT 1 FOR UPDATE OF r SKIP LOCKED`. A mutex-blocked run **stays `Pending`** — `tryQueueRun` (`postgres.go:482+`) hits a unique violation inserting into `mutex_holders` (:546-555) and rolls back (:487), leaving status untouched and emitting nothing. So mutex-blocked runs never enter the claim query's candidate set, and head-of-line blocking cannot occur there. **It occurs one phase earlier:** the Pending snapshot is always the 50 oldest, so ≥51 Pending runs blocked on one held mutex saturate every snapshot and a newer runnable run at position 51+ is never examined at all. Git-unresolved runs consume batch slots identically (`postgres.go:513-518`).

## Facts NOT established — treat as open questions, not givens

- Whether `api.Run.ParentRunID` (`api/types.go:65`) is ever populated. No write path found by grep; absence of evidence, not proof.
- Whether unlocked `RunGitResolver` on N replicas causes observable harm. Absence of the lock is read from source; the consequence is inferred.
- Whether `timeoutMinutes` accepts fractional values end-to-end through YAML schema validation (the Go type is `float64`). **Task 1 must verify this before any scenario depends on it.**
- Tiebreak of `ORDER BY created_at` for identical timestamps.
- Direction and magnitude of clock skew between the agent's `WaitForApproval` deadline and the controller's `timeout_at`.

---

### Task 1: Spec amendment + W2 fixtures

**Files:**
- Modify: `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md`
- Create: `test/edgecase/workloads/call-parent.payload.json`, `call-child.payload.json`, `approval-short.payload.json`, `mutex-hog.payload.json`, `unrelated-probe.payload.json`
- Create: `test/edgecase/tools/bulk-submit.sh`
- Modify: `test/edgecase/scenarios/w1-5-oneway-partition-zombie.md` (stale line citations)

**Interfaces:** Produces every fixture Tasks 2-10 consume.

- [ ] **Step 1: Amend the spec's W2-9 row.** Its premise is inverted (see Verified facts). Replace the row with one describing the real mechanism — the 50-row Pending snapshot in `TransitionPendingToQueued`, not the claim query — keeping the ID `W2-9 (C15)` and invariants `I1, I5`.
- [ ] **Step 2: Fix the stale citations** in `test/edgecase/scenarios/w1-5-oneway-partition-zombie.md` (`:66,:381,:387` cite `postgres.go:1216`/`:1224`; correct to `:1238`/`:1246`). Verify the new numbers by reading the file, do not trust this plan.
- [ ] **Step 3: Verify fractional `timeoutMinutes` end-to-end** before authoring `approval-short.payload.json`. Apply a job with `timeoutMinutes: 0.5` against a running stack (or, if no stack is up, read the DSL parse + validation path and say plainly whether it round-trips). If fractional values are rejected, use `timeoutMinutes: 1` and note that W2-8's window is correspondingly 60s. **Record the answer in the runbook for Task 9 — do not leave it implicit.**
- [ ] **Step 4: Author the fixtures.** All follow the existing `{"yaml":"..."}` envelope, `native: true`, `agentSelector: [kind:linux]`, matching the style of `test/edgecase/workloads/sideeffect.payload.json`.
  - `call-parent.payload.json` — job `edge-call-parent`, one `call:` step invoking `edge-call-child`, plus a preceding native step long enough to make the call step's timing controllable.
  - `call-child.payload.json` — job `edge-call-child`, one native step that runs ~90s and writes a timestamped marker to `/data/child.log` so an orphaned child is observable after its parent dies.
  - `approval-short.payload.json` — job `edge-approval-short`, `before` → `gate` (approval, shortest timeout that round-trips per Step 3) → `after`.
  - `mutex-hog.payload.json` — job `edge-mutex-hog`, `spec.concurrency.mutex: edge-mutex`, one step sleeping ~600s. This is the lock holder for W2-9.
  - `unrelated-probe.payload.json` — job `edge-unrelated-probe`, **no mutex**, `echo probe-ran`. The starvation probe.
- [ ] **Step 5: Author `test/edgecase/tools/bulk-submit.sh`** — POSIX `sh`, `set -eu`, same style as `inject.sh`. Usage `bulk-submit.sh <job-name> <count>`; POSTs `count` triggers to `/api/v1/jobs/<job>/trigger` against `${UNIFIED_SERVER:-http://localhost:18080}` with `${UNIFIED_TOKEN:-ha-admin-token}`, printing each run id one per line so the caller can `tee` it. Must be usable for W2-9's >50-run submission.
- [ ] **Step 6: Verify every payload parses.** Decode each file's `yaml` field and run it through a YAML parser; confirm `spec.concurrency.mutex` (not `spec.mutex`) where a mutex is intended. Paste the verification output into the task report — this is the exact class of bug W1-4 shipped.
- [ ] **Step 7: Commit** (`test(edgecase): W2 fixtures, bulk-submit tool, spec W2-9 reframe`).

---

### Task 2: W2-1 — leader exclusion across every background job

**Files:** Create `test/edgecase/scenarios/w2-1-leader-exclusion.md`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I1

- [ ] **Step 1: Write the runbook.** Stack: plain `test/ha` (3 controllers), no overlay.
  - **Primary probe — `pg_locks` census.** With all three controllers up and idle, `SELECT objid, pid, granted FROM pg_locks WHERE locktype='advisory' ORDER BY objid;` at three sampling points ~30s apart. Expect exactly one holder per key for each of the eleven keys that is actually wired in this stack (note: log archiver / cache cleanup / log trim require `obj != nil`, and audit/run retention require their flags — **determine from the compose env which are actually active and say so; a key with zero holders because the job is not wired is a null result, not a violation**). Map `objid` → job via the Verified-facts table.
  - **Secondary probe — the three unlocked jobs.** Confirm from `pg_locks` that no advisory lock exists for `RunGitResolver`, `DeleteStaleAgents`, or `DeleteExpiredOIDCStates`, then demonstrate concurrent execution: stop one agent so its `last_seen_at` goes stale past 5 minutes, and observe whether the `deleteStaleAgents deleted=N` line (`main.go:393-395`, emitted only when N>0) appears on **more than one** controller for the same deletion. Because the DELETE is idempotent only one replica can report N>0 for a given row — so **also** capture `SELECT count(*) FROM agents` around the deletion and note that a single deleted row cannot distinguish "one replica ran" from "three replicas raced and two saw N=0". State that limitation explicitly rather than over-claiming.
  - **`RunGitResolver` is the interesting one but needs a git-backed job**, which this campaign does not have (W1-7 was deferred for the same reason). **Do not fabricate one.** Record the absence of the lock as a code-read finding with `file:line`, state clearly that concurrent-execution harm was NOT demonstrated live, and carry it to W3 or later as a scenario that needs the git-over-HTTP fixture.
  - **Leader failover.** Kill the controller holding the scheduler lock (`grep "scheduler became leader"` identifies it). Confirm a different controller acquires it, and re-run the `pg_locks` census to confirm every key again has exactly one holder — no key left orphaned, none doubly held.
  - Recording: two replicas simultaneously holding the same advisory key = major (I1). A wired job with no key holder across all samples = major (the job is not running at all). Absence of leader election on the three unlocked jobs = **violation, severity by job**: judge each on whether concurrent execution can produce a wrong result, and say which judgements are inferred rather than demonstrated.
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings + teardown `-v` + commit** (scenario id w2-1).

---

### Task 3: W2-2 — agent replacement (same ID re-registers) with orphaned runs

**Files:** Create `test/edgecase/scenarios/w2-2-agent-replacement.md`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I1, I3

- [ ] **Step 1: Write the runbook.**
  - Trigger `edge-call-parent` so a parent run and a `call:` child are both live and linked. Confirm the link exists: `SELECT run_id, step_index, child_run_id FROM step_reports WHERE child_run_id IS NOT NULL`.
  - **Part A — clean replacement.** `docker compose restart agent1` (the claiming agent). On startup it registers (upsert, no reconciliation) then calls `ReconcileRuns`, which fails **every** Running run under `agent1` with **no grace**. Observe: the parent Failed, the child Cancelled by transitive cascade, `"agent reconcile: failed orphaned run (agent process replaced)"` in controller logs (`api_agent.go:828`), and `mutex_holders`/`named_lock_slots` empty. Time the whole sequence.
  - **Part B — the no-grace consequence.** Trigger a fresh run, wait until it is claimed, and restart the agent **within 5 seconds of the claim**. The startup reconcile has no `claimed_at` grace, so a run claimed one second earlier is failed immediately. Record whether that is what happens and how it is reported — this is the sharpest contrast with the heartbeat path's 60s grace and is the entry's likely payload.
  - **Part C — I3.** After both parts, confirm every mutex/named-lock row is released and a successor run acquires the mutex.
  - Recording: a run left `Running` with no agent = major (I1). A descendant left Running after its parent is failed = major (I3/I1). Immediate failure of a just-claimed run = judge on the documented contract: `postgres.go:271` has no grace by construction, so this is as-designed — likely **observation**, but state the operational cost (a rolling agent restart kills in-flight work with no drain window) prominently.
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings + teardown `-v` + commit** (scenario id w2-2).

---

### Task 4: W2-3 — stuck-run reaper boundary timing

**Files:** Create `test/edgecase/scenarios/w2-3-reaper-boundary.md`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I1, I3, I5

**Interfaces:** Consumes `longrun.payload.json`, `inject.sh nginx-block` (from W1's `oneway.override.yaml`).

- [ ] **Step 1: Write the runbook.**
  - **Anchor the clock properly.** W1's checkpoint established that controller boot+leader election is ~1-2s and that the 26-38s `readyz` gap is rig-side and unexplained. Budget every boundary from the DB (`claimed_at`, `last_seen_at`, `runs.updated_at`) and from the `"scheduler became leader"` log line — **not** from `readyz`.
  - **Arm A — straddle the 90s staleness boundary.** Block the claiming agent at nginx, then sample `SELECT id, claimed_at, last_seen_at, NOW()-last_seen_at FROM ... ` every 5s and record the exact tick at which the run is failed. Expected: eligible at `max(claimed_at+60s, last_seen_at+90s)`, acted on at the next 30s tick. Record the measured latency from eligibility to reap and compare against the 30s tick — a reap later than one full tick past eligibility is worth explaining.
  - **Arm B — straddle the 60s claim grace.** Block the agent **immediately** after the claim so `last_seen_at+90s` fires well before `claimed_at+60s` is met, and confirm the run is NOT reaped until the grace also passes. This isolates which conjunct binds.
  - **Arm C — the `DeleteStaleAgents` interaction (the interesting arm).** Keep the agent blocked past **5 minutes** so its row is deleted. Confirm `SELECT count(*) FROM agents WHERE id='agent1'` goes to 0, then confirm the `a.id IS NULL` branch now matches with no time component. Then trigger a run, let it be claimed, block the agent, and delete the agent row while the run is younger than 90s — does the run become reapable at `claimed_at+60s` alone? Record the answer. **This is the arm most likely to produce a finding**: a run claimed by a since-deleted agent is reapable essentially forever, and the two windows (90s, 300s) being nested means normal operation never exercises it.
  - **Arm D — `failOrphanedRun`'s three non-transactional writes.** `failOrphanedRun` now does `MarkRunStepsInterrupted` → `MarkRunFinished` → `cancelDescendantRuns` with no transaction. Using `edge-call-parent` so a descendant exists, kill the reaping controller (`kill-hard`) repeatedly during reap windows and look for the interleaving where the parent is `Failed` but the child is still `Running`. Because `ListStuckRunIDs` only selects `Running`, nothing re-drives it. **If you cannot hit the window in a bounded number of attempts (cap it — say 10), record that plainly with the attempt count rather than claiming the window is unreachable**; the code-read argument stands on its own and should be filed with its `file:line` evidence and an explicit "not reproduced live" label.
  - Recording: an orphan descendant permanently Running after its parent is reaped = major (I1). A run reapable forever after agent-row deletion = judge against the documented contract in `postgres.go:1264-1274`-style comments; if undocumented, violation. Boundary latencies within one tick = observation.
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings + teardown `-v` + commit** (scenario id w2-3).

---

### Task 5: W2-4 — queued-run reaper grace boundary during full agent outage

**Files:** Create `test/edgecase/scenarios/w2-4-queued-reaper.md`, `test/edgecase/compose/queuedgrace.override.yaml`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I1, I5

- [ ] **Step 1: Create the overlay.** `queuedgrace.override.yaml` sets `UNIFIED_QUEUED_RUN_GRACE=30s` on all three controllers so the scenario runs in bounded time instead of 5 minutes. Do NOT modify `test/ha/docker-compose.ha.yaml`. Verify the controllers log `invalid UNIFIED_QUEUED_RUN_GRACE, using default` **only** if the value is malformed — its absence confirms the override took.
- [ ] **Step 2: Write the runbook.**
  - **Part A — the reap.** Stop both agents. Trigger `edge-tick`. The run goes Pending → Queued (the scheduler still queues it; queueing does not require an agent) and then, past `minAge`, the queued-run reaper fails it. Capture the **system log line** it appends first (`queuedrun_reaper.go:70`, step index `-1`, message `"run failed: no eligible agent available to claim it"`, plus the `(requires agent labels: ...)` suffix when a selector is present) via `GET /runs/{id}/logs` — this is unusually good operator-facing behavior and should be recorded as such if it works.
  - **Part B — the race at the edge (the point of the scenario).** Trigger a run, then bring an agent back **just before** `created_at + grace`. Two outcomes are possible: the agent claims it (run survives) or the reaper fails it first. Run this arm several times with the agent's return walked across the boundary (grace−5s, grace−1s, grace+1s) and record which side wins at each offset. **A run failed while an eligible agent was already live and claiming = major (I1).**
  - **Part C — the `created_at` clock.** The age gate is `created_at`, not a queued-at stamp. Construct a run that sits Pending a long time (block queueing by holding `edge-mutex` with `edge-mutex-hog`, then release it) so that it is already past `minAge` the instant it becomes Queued — and see whether it is reaped before any agent can plausibly claim it. Record the measured window between Queued and reap.
  - **Part D — the deliberate capability gap.** Confirm the documented behavior at `postgres.go:1264-1274`: a run that is label-claimable but capability-unschedulable is left Queued forever and NOT reaped. This is documented intent — record as observation, and note whether anything surfaces it to an operator besides the JobDetail banner the comment mentions.
  - Recording: Part B losing to the reaper with a live eligible agent = major. Part C reaping a run that never had a fair chance = major if the run is failed before one full claim poll could have occurred, else observation with the measured window.
- [ ] **Step 3: Commit runbook + overlay.** **Step 4: Execute.** **Step 5: Findings + teardown `-v` + commit** (scenario id w2-4).

---

### Task 6: W2-5 — `call:` child created but the parent link is never written

**Files:** Create `test/edgecase/scenarios/w2-5-unlinked-child.md`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I1, I3

- [ ] **Step 1: Write the runbook.**
  - **The window** is `callstep.go:50` (child created, `POST .../children` returns) to `:62-66` (`_ = client.ReportStep(... ChildRunID ...)`, error discarded). Two HTTP round-trips. Kill the agent inside it and the link is never written; the in-source comment at `:56-61` claims the terminal report self-heals, which holds only if the agent survives to send one.
  - **Injection.** Two approaches, try the cheap one first: (a) `inject.sh nginx-block agent1` timed to land between the two calls, so the child-create succeeds and the link report is abandoned — note this rides the W1-5 4xx-abandonment major, which makes the abandonment *permanent* and is exactly the realistic path; (b) `kill-hard agent1` timed the same way. **(a) is more likely to hit and more realistic — prefer it, and say why in the runbook.**
  - **Confirm the unlinked state:** the child run exists (`GET /api/v1/runs?jobName=edge-call-child` returns it) but `SELECT child_run_id FROM step_reports WHERE run_id='<parent>'` returns no row. Also check whether `runs.parent_run_id` exists as a column and is populated — the exploration found no write path but labelled it absence-of-evidence; **settle it here** by reading the schema and the row.
  - **Then demonstrate the consequence:** cause the parent to be cascade-cancelled (cancel it, or let the reaper fail it) and show the child **keeps running to completion** — `/data/child.log` keeps growing past the parent's terminal timestamp, and the child reaches `Succeeded` as an orphan with no parent to attribute it to.
  - **Also record the blast radius:** the unlinked child is invisible to all five `cancelDescendantRuns` call sites, and to the WebUI caller→child navigation that `callstep.go:56-61` exists to enable.
  - Recording: an orphan child surviving its parent's cancellation and running to completion = major (I1 — the run is unreachable from the only edge the system has). If the link turns out to be written transactionally after all, that is a finding in the opposite direction — record it and correct the Verified-facts block.
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings + teardown `-v` + commit** (scenario id w2-5).

---

### Task 7: W2-6 — scheduler crash between run creation and `last_fired_at`

**Files:** Create `test/edgecase/scenarios/w2-6-schedule-duplicate-fire.md`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I1, I2

**Interfaces:** Consumes `schedule-every-minute.payload.json`.

- [ ] **Step 1: Write the runbook.**
  - **The window** is `scheduler.go:189` (`CreateRun` returns) to `:193` (`UpdateScheduleLastFiredAt`) — two separate pool connections, no transaction. It is single-digit milliseconds (inferred, not measured), so hitting it with `kill-hard` is a repetition game.
  - **Arm A — race the window.** Apply `edge-every-minute`. In a loop (cap the attempts — say 20 — and **report the count whether or not you hit it**), `kill-hard` the leader controller as close as possible to the top of each minute, then check for two runs of `edge-tick` with `triggeredBy='schedule:edge-every-minute'` for the same cron occurrence. Identify duplicates by `created_at` falling in the same minute.
  - **Arm B — the deterministic variant.** The same defect is reachable without winning a millisecond race: `UpdateScheduleLastFiredAt` failing is logged at Warn and **not retried** (`:195`). Kill Postgres (`inject.sh kill-hard postgres`) so that `CreateRun` succeeds against a connection that is still good while the subsequent update fails — or, more reliably, observe the *converse*: after any controller kill, compare `schedules.last_fired_at` against the newest `edge-tick` run's `created_at` and record whether they ever disagree by a full cron period. **A disagreement is the fingerprint of the window regardless of how it was produced.** Sample this continuously across the whole scenario rather than only at kill instants.
  - **Arm C — the silent-skip path.** Stop all controllers for **over an hour**? No — do not. Instead verify the code path by inspection and construct the cheap equivalent: set a schedule's `last_fired_at` far in the past directly via psql, then observe that the next tick advances it with **no run created and no log line at all** (`:197-201`). Record this as the missed-fire blind spot. **Note explicitly that this arm mutates DB state directly rather than injecting a fault, and is therefore a demonstration of the code path, not a naturally-occurring observation.**
  - Recording: two runs for one cron occurrence = major (I2 — a schedule firing twice is a duplicate side effect). `last_fired_at` diverging from the last created run = major or minor depending on whether it self-corrects on the next tick. The silent skip in Arm C = the W0 catch-up-window major already filed — **cross-reference it, do not re-file.**
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings + teardown `-v` + commit** (scenario id w2-6).

---

### Task 8: W2-7 — two live agent processes sharing one agent ID

**Files:** Create `test/edgecase/scenarios/w2-7-duplicate-agent-id.md`, `test/edgecase/compose/dupagent.override.yaml`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I1

- [ ] **Step 1: Create the overlay.** `dupagent.override.yaml` adds a fourth agent service (`agent1b`) configured with **the same agent ID and credentials as `agent1`**. Read `test/ha/docker-compose.ha.yaml` for how agent1 is configured and mirror it exactly except the service name. Do not modify the ha compose file.
- [ ] **Step 2: Write the runbook.**
  - **Part A — startup reconcile (the fast, deterministic trigger).** Start `agent1` alone, trigger `edge-longrun`, wait for the claim, then start `agent1b`. Its startup `ReconcileRuns` has **no grace** and fails **every** Running run under `agent1` — including one claimed a second ago. Expect controller log `"agent reconcile: failed orphaned run (agent process replaced)"` (`api_agent.go:828`). Confirm the run is Failed while `agent1`'s process is **still executing the step** — the same zombie shape as W1-5.
  - **Part B — heartbeat mutual annihilation (the steady state).** With both processes live, trigger two runs so each process claims one. After the 60s heartbeat grace, each process's heartbeat enumerates all `agent1` Running runs older than 60s and fails those absent from its own snapshot — so each reaps the other's run. Confirm both runs Failed while both processes keep executing. **Note that a successful heartbeat reconcile logs nothing** (`api_agent.go:115` logs only on error), so attribute it via `runs.updated_at` and `step_reports` rather than logs.
  - **Part C — the ownership guard.** Confirm that both processes pass `agentRunGuard`'s bare string compare (`agent_guard.go:121`) — i.e. process B can post step reports and logs against a run process A claimed. Demonstrate it directly if you can do so without product changes; if not, record it as a code-read claim with `file:line` and label it as such.
  - Recording: mutual annihilation of legitimate runs = major (I1). Two processes both accepted as the owner of one run = major. The absence of any fencing token = record as the root cause; note that `UpsertAgent`'s `ON CONFLICT DO UPDATE` means the collision is **silently invisible** to operators, which is the operationally dangerous part.
- [ ] **Step 3: Commit runbook + overlay.** **Step 4: Execute.** **Step 5: Findings + teardown `-v` + commit** (scenario id w2-7).

---

### Task 9: W2-8 — approval decision racing the timeout boundary

**Files:** Create `test/edgecase/scenarios/w2-8-approval-race.md`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I7

**Interfaces:** Consumes `approval-short.payload.json` (Task 1).

- [ ] **Step 1: Write the runbook.**
  - **The window is wide and does not require winning a race.** Per `approval.go:17-20`, when the agent's local deadline expires it fails the step and the controller-side `run_approvals` row **stays `Pending`**, because the agent has no decision endpoint. So: let the gate time out on the agent, confirm the run is `Failed`, and confirm `SELECT status FROM run_approvals` is still `Pending`. That state is the vulnerability, and it persists until the 1-minute approval reaper happens to tick.
  - **Then fire the decision.** `POST /api/v1/runs/{id}/approvals/{stepIndex}` with `{"decision":"approve"}`. Per `DecideApproval` (`postgres.go:2439-2450`) the UPDATE guards only on the approval row's status, so expect **204** and `run_approvals.status='Approved'` **on a run whose own status is `Failed`**. Capture both rows in one psql read so the contradiction is in a single artifact.
  - **Capture the audit trail.** `GET /api/v1/audit` should hold `action=run.approval.decide, resource=<the Failed run id>, status=204`. That row is written synchronously (`audit.go:222-227`), so no polling is needed. **This is the entry's strongest evidence: an audit log asserting a human approved a run that had already failed.**
  - **Also probe the reaper race.** Repeat with the decision timed to land in the same 1-minute window the reaper is scanning, and record which writer wins (both CAS on `status='Pending'`, so exactly one can). Record the observed distribution over several attempts rather than a single outcome.
  - **Measure the two clocks.** Capture `run_approvals.timeout_at` (controller clock, `api_approvals.go:86-89`) and the agent's `"approval timed out"` log timestamp (agent clock, `approval.go:64-67`) and record the skew. The exploration inferred the agent's deadline is normally the later of the two but did not measure it — **settle it here**.
  - Recording: `Approved` written onto a terminal run + a 204 audit row = major (I7 — the recorded state contradicts reality, and it is an *audit* record, which is the one thing that must not lie). Note whether anything anywhere surfaces the contradiction to an operator.
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings + teardown `-v` + commit** (scenario id w2-8).

---

### Task 10: W2-9 — Pending-snapshot head-of-line blocking

**Files:** Create `test/edgecase/scenarios/w2-9-head-of-line.md`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I1, I5

**Interfaces:** Consumes `mutex-hog.payload.json`, `unrelated-probe.payload.json`, `bulk-submit.sh` (Task 1).

- [ ] **Step 1: Write the runbook.**
  - **State the corrected mechanism up front** — the original spec row described the claim query, which is the wrong phase. The blocking is in `TransitionPendingToQueued`'s `ORDER BY created_at LIMIT 50` snapshot (`postgres.go:437-441`, limit from `scheduler.go:58`); mutex-blocked runs stay `Pending` (`tryQueueRun` rolls back on the `mutex_holders` unique violation, `postgres.go:546-555`) and therefore re-fill every snapshot.
  - **Setup.** Trigger `edge-mutex-hog` and confirm it holds `edge-mutex` (`SELECT * FROM mutex_holders`). Then `bulk-submit.sh edge-sideeffect 55` (all share `edge-mutex`, so all block). Confirm `SELECT count(*) FROM runs WHERE status='Pending'` ≥ 51 and that they are the 55 oldest.
  - **The probe.** Trigger `edge-unrelated-probe` (no mutex) with a free agent idle. Poll its status every 5s for 3 minutes. **Expected under the hypothesis: it never leaves `Pending`.** Capture the full poll series, `SELECT id, job_name, status, created_at FROM runs ORDER BY created_at` at start and end, and the idle agent's logs showing it claiming nothing.
  - **Falsify carefully.** Then bulk-cancel enough blocked runs to drop the Pending count below 50 and confirm the probe is queued and claimed promptly. That transition is what turns a plausible story into a demonstrated threshold. **Record the exact Pending count at which the probe unblocked** — if it is not 50, the model is wrong and the real number is the finding.
  - **Record the amplifier:** git-unresolved runs consume snapshot slots identically (`postgres.go:513-518`), so the same starvation is reachable without any mutex.
  - Recording: an unrelated runnable job never claimed while an agent idles = major (I1 and I5 — recovery is not bounded; it never happens). If the probe *is* eventually claimed, record the latency and downgrade to observation with the measured number.
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings + teardown `-v` + commit** (scenario id w2-9).

---

### Task 11: W2 checkpoint

**Files:** Modify `test/edgecase/FINDINGS.md`, `test/edgecase/README.md`

- [ ] **Step 1: Append `## Checkpoint: W2 complete`** following the W1 checkpoint's format and classification rule: scenarios run, violations vs observations with severity breakdown, per-scenario counts, and forward-looking impact. At minimum state: (a) which W3+ scenarios the reaper/queue findings change; (b) whether the three unlocked background jobs need their own scenario in a later wave and which; (c) whether `RunGitResolver` should be folded into W3's git fixture work.
- [ ] **Step 2: Copy the wave's raw evidence** from the session scratchpad to the evidence root (`<project parent>/edgecase-evidence/w2/`), per the convention in `test/edgecase/README.md`. Verify with `diff -r`.
- [ ] **Step 3: Commit** (`test(edgecase): record W2 checkpoint`).
