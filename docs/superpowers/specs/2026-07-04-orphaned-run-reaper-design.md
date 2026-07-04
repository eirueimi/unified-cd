# Design: Orphaned-run reaper + agent heartbeat + k8s orphan-pod GC

**Date:** 2026-07-04
**Status:** Approved (pending implementation plan)

## Problem

When an agent dies (crash / network partition / kill) **while executing a run**,
that run is stranded in `Running` forever:

- The run was already claimed (`claimed_by` = the dead agent, status `Running`),
  so no other agent picks it up (claims only take `Pending`→`Running` work).
- Nothing fails or re-queues it. The existing periodic workers only cover other
  cases: `RunScheduler` (Pending→Queued) and `RunApprovalReaper` (approval
  timeouts). There is **no reaper for stuck `Running` runs**.
- On the k8s agent, the pod also **leaks** — the agent's deferred `DeletePod`
  never runs, so `ucd-run-*` pods accumulate on the cluster.

This is distinct from the already-fixed TODO #1 (a *live* agent wedging itself on
an invalid status report). Both are needed for "a run never hangs forever."

### Root blocker: there is no reliable liveness signal

`agents.last_seen_at` is **not** a dependable "agent alive" signal today. It is
updated only on **claim** (`UpsertAgent` on claim, per TODO #12) and registration.
The agent's slot loop is `Claim(30s) → executeRun → Claim…` — while a slot is
inside `executeRun` it does not poll. So when **all** `MaxConcurrent` slots are
busy executing long runs, the agent stops polling and `last_seen_at` goes stale
even though the agent is perfectly alive. `TouchAgent` (`last_seen_at = NOW()`)
exists but is never called on a timer.

Consequence beyond the reaper: `cmd/controller/main.go` already runs
`DeleteStaleAgents(5m)`, so a **busy-but-alive** agent can currently be *deleted*
from inventory after 5 minutes — a pre-existing latent bug. A real heartbeat
fixes both problems.

## Goal

Detect a dead agent reliably and (1) fail its stranded `Running` runs with a
clear reason, and (2) garbage-collect its leaked k8s pods — without false-positives
against healthy, saturated agents, and without re-running steps that may have
side effects.

## Decisions (from brainstorming)

| Question | Decision |
|---|---|
| Liveness signal | **Add a dedicated agent→controller heartbeat** (independent of claim polling). Without it, no reaper threshold is safe. |
| Reaper action on a stranded run | **Fail it** (`Failed`, reason "agent lost"). NOT re-queue — re-running partially-executed steps risks duplicate side effects (deploys, etc.). Matches GitHub Actions runner-loss behavior. |
| k8s orphan pods | **GC in scope**, implemented in the **k8s-agent** (the controller has no cluster access). |
| Reaper placement | Controller, leader-elected via advisory lock, mirroring `RunApprovalReaper`. |

## Architecture

```
 agent (both standard + k8s):
   heartbeat goroutine ──POST /api/v1/agents/{id}/heartbeat──▶ TouchAgent (last_seen_at=NOW())
     (every ~heartbeatInterval, independent of claim polling)

 controller:
   RunStuckRunReaper (leader-elected, every ~interval):
     FailStuckRuns(staleAfter):  Running runs whose claimed_by agent is missing
       OR last_seen_at < NOW()-staleAfter, AND claimed_at < NOW()-grace  →  MarkRunFinished(Failed)

 k8s-agent:
   orphan-pod GC (every ~interval):
     list ucd-run-* pods → for each, GetRun(runID) → if run is terminal or gone → DeletePod
```

### 1. Agent heartbeat — `internal/agent` + `internal/k8sagent` + controller

- **Controller endpoint:** `POST /api/v1/agents/{agentId}/heartbeat` under
  `BearerAuth(AgentToken)` → `store.TouchAgent(agentID)` → 204. (Reuses the
  existing, currently-unused `TouchAgent`.)
- **Agent client:** `Client.Heartbeat(ctx, agentID) error` (standard agent) and
  the equivalent on the k8s-agent's agentlib client (they share `agentlib`).
- **Agent loop:** in `Agent.Run` (and the k8s-agent's run loop), start ONE
  background goroutine that ticks every `heartbeatInterval` (default 15s) and
  calls `Heartbeat`, until the run context is cancelled. Best-effort: a failed
  heartbeat logs a warning and retries next tick (never crashes the agent).
- This keeps `last_seen_at` fresh regardless of slot saturation, fixing both the
  reaper's signal and the `DeleteStaleAgents` false-deletion of busy agents.

### 2. Stuck-run reaper — `internal/controller/stuckrun_reaper.go`

Modeled on `internal/controller/approval_reaper.go`:

- `RunStuckRunReaper(ctx, st, interval)` — a ticker loop; each tick acquires a
  dedicated advisory lock (`stuckReaperLockKey`, a new constant distinct from the
  approval reaper's) so exactly one controller reaps; on lock-miss it is a
  follower and does nothing this tick.
- As leader, calls a new store method:
  `FailStuckRuns(ctx, staleAfter, grace time.Duration) (int, error)` which, in one
  statement, marks `Failed` every run that is:
  - `status = 'Running'`, AND
  - `claimed_at < NOW() - grace` (don't reap a run claimed seconds ago before its
    agent's first heartbeat), AND
  - its `claimed_by` agent row is **missing** (LEFT JOIN — covers an agent already
    removed by `DeleteStaleAgents`) OR `agents.last_seen_at < NOW() - staleAfter`.
  Implementation: `UPDATE runs SET status='Failed', ... WHERE id IN (SELECT r.id FROM runs r LEFT JOIN agents a ON r.claimed_by = a.id WHERE r.status='Running' AND r.claimed_at < NOW()-$grace AND (a.id IS NULL OR a.last_seen_at < NOW()-$stale)) RETURNING id`. Log the count.
  - Reuse the existing terminal-run finalization path if `MarkRunFinished`
    performs side effects (e.g. NOTIFY / mutex release / semaphore release); if
    `MarkRunFinished` does more than a status update, the reaper should iterate the
    selected run IDs and call `MarkRunFinished(id, Failed)` per run instead of a
    bulk UPDATE, so those side effects fire. The plan will verify which and choose.
- **Config/wiring:** `cmd/controller/main.go` starts it like the approval reaper:
  `go controller.RunStuckRunReaper(ctx, pg, interval)`. Values: `interval` ~30s,
  `staleAfter` ~90s (≈6 missed 15s heartbeats), `grace` ~60s. Constants in the
  reaper file (or controller config), documented.

### 3. k8s orphan-pod GC — `internal/k8sagent`

- A periodic GC (every ~1m) in the k8s-agent: list run pods (label
  `app=unified-cd-agent`, the selector already used in `pool.go:194`), extract
  `unified-cd/runId` from each pod's labels, call `a.client.GetRun(runID)`:
  - run **terminal** (`Succeeded`/`Failed`/`Cancelled`) or **not found** → the pod
    is orphaned → `DeletePod`.
  - run still active → leave it.
- Guard against pool pods: pods that are part of the warm pool (annotated
  `annoPoolStatus`) and idle should be left to the pool's own idle-timeout logic —
  the GC only removes pods bound to a terminal/absent run. The plan will reconcile
  with the pool's reuse annotations so the GC never deletes a healthy pooled pod.
- Runs as its own goroutine started where the k8s-agent starts its other loops.

### 4. Tests

- **Store** (`FailStuckRuns`): a Running run claimed by a stale/absent agent →
  gets Failed; a Running run claimed by a fresh agent → untouched; a run claimed
  within `grace` → untouched; a Pending run → untouched. (Postgres-backed store
  test, matching the package's harness.)
- **Reaper** (unit/leader): the loop, given a store, fails the stale run and logs
  the count; follower (lock held) is a no-op. Mirror `approval_reaper_test.go`.
- **Heartbeat**: controller endpoint test (`POST .../heartbeat` → 204 → `last_seen_at`
  advanced); agent-side test that the heartbeat goroutine calls the client on its
  tick (fake client + a short interval).
- **k8s GC** (cluster-free): a fake pod lister + fake run-status client — a pod
  whose run is terminal is deleted; a pod whose run is active is kept; a pooled/idle
  pod is not touched. (Extract the GC decision into a pure function seam like the
  existing `orchestrate` testable core.)
- **e2e/integration** (best-effort, may need Postgres/Docker): submit a run,
  "kill" its agent (stop heartbeating), assert the run reaches `Failed` within the
  bound. Skip when the harness is unavailable.

### 5. Docs

`docs/high-availability.md`: document the heartbeat + stuck-run reaper (runs no
longer hang on agent death; bounded by `staleAfter`), and that k8s orphaned pods
are GC'd. Note the reaper Fails (does not re-queue) stranded runs, and why.
Update the TODO.md item A to "implemented".

## Non-goals (YAGNI / separate)

- Re-queue/retry of stranded runs (Fail is the chosen semantic).
- Per-step checkpoint/resume.
- A general work-stealing scheduler.
- Changing `DeleteStaleAgents` (the heartbeat already prevents its false-positive;
  its 5m threshold stays, and should remain **larger** than the reaper's
  `staleAfter` so the reaper acts on a still-present agent row in the common case —
  the LEFT JOIN handles the case where it doesn't).

## Touch points

| Path | Change |
|---|---|
| `internal/controller/server.go` | register `POST /agents/{agentId}/heartbeat` |
| `internal/controller/api_agent.go` | `handleAgentHeartbeat` → `TouchAgent` |
| `internal/controller/stuckrun_reaper.go` (new) | `RunStuckRunReaper` (leader-elected) |
| `internal/store/postgres.go`, `store.go` | `FailStuckRuns(staleAfter, grace)` (+ interface) |
| `cmd/controller/main.go` | start the reaper goroutine |
| `internal/agent/agent.go`, `internal/agent/client.go` | heartbeat goroutine + `Client.Heartbeat` |
| `internal/k8sagent/agent.go` (+ agentlib client) | heartbeat goroutine + orphan-pod GC loop |
| `internal/k8sagent/podmanager.go` | pod lister for GC (if not already present) |
| tests + `docs/high-availability.md` + `TODO.md` | as above |

## Acceptance criteria

- Both agents send a periodic heartbeat independent of claim polling; a fully
  saturated but alive agent keeps a fresh `last_seen_at` (and is not falsely
  deleted by `DeleteStaleAgents`).
- The stuck-run reaper (leader-elected) marks a `Running` run `Failed` ("agent
  lost") when its claiming agent's heartbeat is stale (or the agent row is gone),
  respecting a `grace` window; a healthy run and a just-claimed run are untouched.
- The k8s-agent GCs `ucd-run-*` pods whose run is terminal or absent, without
  deleting healthy pooled pods.
- Reaper Fails, never re-queues.
- Cluster-free unit tests cover `FailStuckRuns`, the reaper leader/follower, the
  heartbeat endpoint + agent goroutine, and the k8s GC decision; docs updated.
- `go build ./...` and `go test ./...` pass.
