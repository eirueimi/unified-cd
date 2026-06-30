# Design: Manual approval step (`approval`)

**Date:** 2026-06-30
**Status:** Approved (pending implementation plan)

## Problem

unified-cd has no manual approval gate. For CD use cases (deploy to production,
promote a release) a pipeline needs to pause and wait for a human to approve
before continuing. Today every step runs automatically once dispatched.

## Goal

A new `approval` step type that pauses the run until an authenticated user
approves or rejects it (or it times out). On approval the run continues; on
rejection/timeout the step fails — and because the run then fails, the
`finally` block (already implemented) runs for rollback/notify.

## Key decisions (from brainstorming)

| Question | Decision |
|---|---|
| Who can approve | **Any authenticated principal** (PAT or SSO user). No RBAC dependency. The decider's identity is recorded. |
| Wait mechanism | **Agent-blocking poll.** The agent executing the run blocks on the approval step, polling the controller for a decision (reuses the cancel-poller pattern). Preserves workspace/build artifacts. |
| On reject | **Step fails → run Failed → `finally` runs.** Reuse the existing `Failed` run status (no new run-status enum); the approval outcome is distinguished in the `run_approvals` table. |
| Timeout | **Optional `timeoutMinutes`, default 60.** On timeout the step is treated as rejected (TimedOut) and fails. Agent enforces the deadline locally. |
| Delivery surface | **API + CLI + Web UI button.** |
| Recorded metadata | **Approver identity + optional comment** (+ decision + timestamp). |

## Non-goals (YAGNI)

- RBAC-restricted approvers (any authenticated principal can decide in v1).
- Controller-side pause/re-dispatch (the agent holds the run while waiting).
- Controller-side timeout reaping if the agent dies mid-wait (documented v1
  limitation — see Edge cases).
- A distinct `Rejected`/`TimedOut` **run** status (reuse `Failed`).
- Multiple required approvers / quorum.

## Existing architecture this builds on

- A run is claimed in full (all stages) by one agent and executed in one
  workspace ([internal/agent/agent.go](../../../internal/agent/agent.go)).
- The agent already polls `GetRun` every 5s to detect cancellation
  (`cancelledByMaster`), and reports per-step status via `ReportStep`.
- Steps are compiled by the controller into `api.ClaimStep`
  ([internal/controller/api_agent.go](../../../internal/controller/api_agent.go)).
- The `finally` block runs after the main DAG on failure/cancel
  (`agent.go`, frozen-status runner).
- Step actions are mutually-exclusive typed fields on `StepEntry`
  (`run`/`call`/`uses`/`cache`/`uploadArtifact`/`downloadArtifact`)
  ([internal/dsl/types.go](../../../internal/dsl/types.go)).
- Auth: `ServerAuth` middleware exposes the authenticated principal (PAT name
  or OIDC `sub`/`email`) via the request context; the agent API uses a
  separate agent token.

## Design

### 1. DSL — the `approval` step action

Add a mutually-exclusive `Approval *ApprovalStep` action to `StepEntry`/`Step`.

```yaml
steps:
  - name: build
    run: make build
  - name: gate
    if: '{{ eq .Params.env "production" }}'   # optional; approval only when true
    approval:
      message: "Deploy to production?"          # optional, shown to the approver
      timeoutMinutes: 60                         # optional, >0, default 60
  - name: deploy
    run: make deploy
```

```go
type ApprovalStep struct {
    Message        string  `yaml:"message,omitempty"`
    TimeoutMinutes float64 `yaml:"timeoutMinutes,omitempty"` // default 60 when 0
}
```

Validation (in `validateStepEntries` / `validateStepFull`):
- `approval` counts as one action; mutually exclusive with `run`/`call`/`uses`/`cache`/artifacts.
- `timeoutMinutes` must be `>= 0` (0 → default applied at compile/agent time).
- Allowed in both `steps` and `finally`? **No** — reject `approval` in `finally`
  (a cleanup block must not block on a human). Add to the finally-rejection
  set alongside `cache`/`post`.
- `if:` is allowed on an approval step (skips the gate when false).

### 2. Wire types & controller compile

- `api.ClaimStep` gains `Approval *ClaimApproval { Message string; TimeoutMinutes float64 }`.
- `buildOneClaimStep` copies the approval config (applying the default
  timeout of 60 when unset) so the agent knows the step is a gate.

### 3. Store — `run_approvals`

New migration. One row per approval step instance:

```sql
CREATE TABLE run_approvals (
    run_id      UUID NOT NULL,
    step_index  INT  NOT NULL,
    step_name   TEXT NOT NULL,
    message     TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL,              -- Pending | Approved | Rejected | TimedOut
    decided_by  TEXT,                       -- principal name / email; 'system' for timeout
    comment     TEXT,
    created_at  TIMESTAMPTZ NOT NULL,
    timeout_at  TIMESTAMPTZ,                -- created_at + timeoutMinutes
    decided_at  TIMESTAMPTZ,
    PRIMARY KEY (run_id, step_index)
);
```

Store methods:
- `CreatePendingApproval(ctx, runID, stepIndex, stepName, message, timeoutAt)` —
  idempotent upsert keyed on `(run_id, step_index)`; leaves an existing
  non-Pending row untouched (so re-claim re-wait is safe).
- `DecideApproval(ctx, runID, stepIndex, decision, decidedBy, comment)` —
  conditional `UPDATE ... WHERE status='Pending'` (first writer wins); returns
  whether a row was updated (false → already decided / not found).
- `GetApproval(ctx, runID, stepIndex)` and `ListRunApprovals(ctx, runID)`.

### 4. Step / run status

- New **step** status string `WaitingApproval` (run stays `Running`).
- No new run status; reject/timeout → step `Failed` → run `Failed`.

### 5. Agent — shared `WaitForApproval` helper

A self-contained helper in `internal/agent` (used by BOTH the standard agent
and, after parity, the k8s-agent):

```go
// WaitForApproval blocks until the run's approval at stepIndex is decided or
// the deadline passes. Returns (approved bool). On deadline it records a
// TimedOut decision via the client and returns false.
func WaitForApproval(ctx context.Context, c *Client, agentID, runID string,
    stepIndex int, deadline time.Time) (bool, error)
```

Integration in the step runner:
1. On an approval step, report `ReportStep(status="WaitingApproval", ...)`.
   The controller, on receiving `WaitingApproval`, creates the `run_approvals`
   Pending row from the stored spec (message + timeout for that step index) —
   no extra agent endpoint needed for creation.
2. Call `WaitForApproval` with `deadline = now + timeoutMinutes`. It polls the
   existing agent-accessible `GetRun` (extended to carry each step's approval
   decision) every few seconds — the same read the cancel poller already uses.
3. Approved → report step `Succeeded`, continue. Rejected/TimedOut → record
   the failure (same `recordFailure` path) and report step `Failed`; the run
   fails and `finally` runs.

In the standard agent the helper is invoked from the `makeStepRunner` body
before the run/exec branch; in the k8s-agent it is invoked from `runOneStep`.

### 6. API

- **Decision (management API, `ServerAuth`):**
  `POST /api/v1/runs/{runId}/approvals/{stepIndex}`
  body `{ "decision": "approve" | "reject", "comment": "..." }`.
  `decided_by` is taken from the authenticated principal (not the body).
  → `DecideApproval`. Returns 200 on success, 404 if no pending approval at
  that (run, step), 409 if already decided.
- **Read:** extend `GET /api/v1/runs/{runId}` to include the run's approvals
  (status, message, decided_by, comment) for UI/CLI display.
- **Agent poll:** the agent reads the decision via the existing `GetRun`
  read (already used by the cancel poller and accessible to the agent token),
  extended to carry each step's approval decision. No new human-facing surface.

### 7. CLI

- `unified-cd approve <runId> [--step <name|index>] [--comment "..."]`
- `unified-cd reject  <runId> [--step <name|index>] [--comment "..."]`
- If the run has exactly one pending approval, `--step` may be omitted.
- `unified-cd get run <id>` / `runs` surface the `WaitingApproval` state and
  the approval message.

### 8. Web UI

- `RunDetail.svelte`: when a step is `WaitingApproval`, render the message,
  an optional comment input, and **Approve** / **Reject** buttons that call
  the decision endpoint. After a decision, show `decided_by` + comment.

### 9. Edge cases

- **Double decision / race:** conditional `UPDATE ... WHERE status='Pending'`
  → first writer wins; subsequent calls get 409.
- **Agent death while waiting:** rely on existing agent-loss handling. The
  Pending row persists; on re-claim, `ReportStep(WaitingApproval)` upserts
  idempotently and the agent re-waits. Controller-side timeout reaping is a
  documented v1 limitation.
- **`if:` false on the approval step:** the step is skipped via existing `if:`
  logic — no approval required.
- **Timeout:** enforced agent-side from `timeout_at`; the agent records a
  `TimedOut` decision (`decided_by='system'`) and fails the step.

### 10. Implementation order

`approval` depends on the agent execution path and wires into BOTH agents via
the shared `WaitForApproval` helper. Therefore:

1. **First: k8s-agent parity** (`if:`/`finally`) — separate, already-designed
   work that aligns the k8s-agent loop with the standard agent and gives it a
   place to call `WaitForApproval` cleanly. (Its own plan.)
2. **Then: approval step** — this spec.

## Touch points summary

| Area | Change |
|---|---|
| `internal/dsl` (types.go, parse.go) | `ApprovalStep` action + validation; reject in `finally` |
| `internal/api/types.go` | `ClaimStep.Approval`; approval fields on run read |
| `internal/controller/api_agent.go` | compile approval into ClaimStep; create Pending row on `WaitingApproval` report |
| `internal/controller` (new api_approvals.go) | decision endpoint; run read includes approvals |
| `internal/store` (migration + methods) | `run_approvals` table; Create/Decide/Get/List |
| `internal/agent` | `WaitForApproval` helper; wire into standard agent step runner |
| `internal/k8sagent` | wire `WaitForApproval` into `runOneStep` (after parity) |
| `internal/cli` + `cmd/unified-cli` | `approve` / `reject` commands |
| `web/src/routes/RunDetail.svelte` | approval message + Approve/Reject buttons |
| `docs/jobs.md`, schema | document `approval`; regenerate |
