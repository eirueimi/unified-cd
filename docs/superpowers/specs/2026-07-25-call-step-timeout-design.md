# Call-step timeout: make it configurable, default unlimited

Date: 2026-07-25
Status: Approved (design)

## Problem

A `call:` step launches a child run and polls until it completes. The poller
in `ExecuteCallStep` (`internal/agent/callstep.go`) carries a hardcoded
`const maxWait = 30 * time.Minute`. This cap:

1. **Is not configurable** — there is no env var or DSL field to change it.
2. **Shadows the existing per-step timeout.** The step already runs under a
   `stepCtx` that has the step's `timeoutMinutes` applied to it
   (`internal/agent/orchestrator.go`, where `step.TimeoutMinutes > 0 &&
   step.Retry == nil` wraps `stepCtx` with `context.WithTimeout`). The
   hardcoded 30-minute deadline is layered *on top* of that context, so the
   effective call-step timeout is `min(timeoutMinutes, 30m)`: a user who sets
   `timeoutMinutes: 120` still gets cut off at 30 minutes, and a user who sets
   nothing gets an implicit, undocumented 30-minute limit.

The 30-minute clock starts when the child run is *created* and includes the
child's entire lifecycle — including any time the child spends in `Pending`
(concurrency-lock wait) or `Queued`. A child blocked on a mutex/semaphore can
therefore make the parent call-step fail at 30 minutes even though the child
itself has no wait-timeout.

## Goal

`call:` steps should honor the existing `timeoutMinutes` step field, and
**default to unlimited wait** when it is not set — consistent with how every
other step type already treats `timeoutMinutes` via `stepCtx`.

## Design

### Single change: `internal/agent/callstep.go`

Remove the hardcoded cap and rely solely on the incoming `ctx` (which is the
orchestrator's `stepCtx`):

- Delete `const maxWait = 30 * time.Minute` and the `deadline` variable and the
  `if time.Now().After(deadline)` branch.
- The poll loop continues until the child run reaches a terminal state
  (`Succeeded` → return outputs; `Failed`/`Cancelled` → return a step error).
- The only cut-off is `ctx.Done()`:
  - `timeoutMinutes` set → `stepCtx` fires its deadline at that time
    (`orchestrator.go` already applies it), and the step fails.
  - `timeoutMinutes` unset → `stepCtx` has no deadline → the loop waits
    indefinitely until the child finishes, or until the run is cancelled / the
    job-level timeout fires.
- When `ctx.Err()` is `context.DeadlineExceeded`, wrap it in a clear,
  call-specific message, e.g.
  `call: child run %s did not complete within the step timeout`.
  When `ctx.Err()` is `context.Canceled`, return it as-is (run cancelled).

The `childRunID` return contract is unchanged: it is still returned on every
non-created path (success, failure, cancellation, timeout) so the caller→child
link is preserved on the terminal step report.

### Child-run cancellation on timeout: rely on the existing cascade

No new cancellation code is added.

- The controller's cancel endpoint (`POST /api/v1/runs/{id}/cancel`) requires a
  `developer` user role and is **not** reachable with an agent token, and the
  calling agent does not own the child run, so the agent cannot cancel the
  child directly.
- When the call-step times out it returns an error → the parent step fails →
  the parent run finishes `Failed` → `handleAgentFinishRun`
  (`internal/controller/api_agent.go`) already calls `cancelDescendantRuns`,
  which cancels descendant runs (both `Queued` and `Running`; a `Running` child
  is stopped by its own agent's cancel poller). The child is therefore
  cancelled shortly after the parent run finalizes.

### Scope / blast radius

- `ExecuteCallStep` is backend-agnostic and shared by the host agent and the
  k8s agent, so both get the fix automatically.
- No new env var, no new DSL field, no API/protocol change.

## Testing

- `timeoutMinutes` set on a call step → the step fails at approximately that
  duration (use a small duration / injected clock or direct ctx cancellation to
  keep the test fast).
- `timeoutMinutes` unset → the poller keeps waiting past the old 30-minute cap;
  verify it does not self-abort (assert it is still polling, or drive
  termination via child completion / ctx cancel rather than an internal
  deadline).
- Update existing tests that assume the 30-minute behavior (e.g.
  `internal/agent/agent_callrun_test.go`, whose comment references "up to 30
  minutes").
- Confirm the timeout path still returns a non-empty `childRunID` so the
  caller→child link is reported.

## Documentation

- Document that a `call:` step honors `timeoutMinutes` and that, when unset,
  there is no timeout (the step waits until the child run finishes or the run
  is cancelled). Update the DSL / configuration docs accordingly.

## Known limitations (out of scope)

- **Failure-tolerant parents.** If the parent run swallows the timed-out call
  step's failure (e.g. downstream `if: failure()` recovery, or the call step
  sits in a construct that lets the run finish `Succeeded`), the parent-finish
  cascade — which only fires on `Failed`/`Cancelled` — will not cancel the
  child, and the child can linger. Making cancellation immediate at timeout
  would require a new agent-authenticated "cancel child" endpoint authorized via
  the parent→child link; that was explicitly deferred.
- **`call` + `retry` + `timeoutMinutes` together.** The orchestrator skips the
  whole-step `stepCtx` timeout when `step.Retry != nil`, and the per-attempt
  timeout wraps only the `run:` branch, not `ExecuteCallStep`. A call step that
  sets both `retry` and `timeoutMinutes` would therefore still wait
  indefinitely. This is a pre-existing quirk and is not addressed here.
