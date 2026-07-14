# Step-Level Automatic Retry

**Status**: Draft for review
**Date**: 2026-07-14

## Problem

A flaky step (a network hiccup, a slow-to-start service, a transient test failure)
fails a whole run today, even when re-running it once would succeed. The DSL has
`continueOnError` (tolerate a failure) but no way to *re-run* a failing step.
Job authors want a step to automatically retry up to N times before the run is
considered failed.

Goal: a step declares `retry: {attempts, backoff}`; the agent orchestrator
re-runs the step's body on failure, up to `attempts` total tries, with a fixed
`backoff` wait between tries.

## Design

### DSL

Add an optional `retry` object to a step (both the flat `Step` and the
`StepEntry`/parallel forms):

```yaml
- name: flaky-test
  run: go test ./...
  retry:
    attempts: 3      # total tries (1 = no retry, the default). 3 = 1 initial + up to 2 retries
    backoff: 30s     # fixed wait after each failed try before the next (omitted/0 = immediate)
```

`dsl.RetrySpec`:
- `Attempts int` — total number of tries. Must be `>= 1`. `1` means "no retry"
  (equivalent to omitting `retry`). A `retry:` block with `attempts` unset or
  `< 1` is a parse/validation error.
- `Backoff string` — a Go duration (`"30s"`, `"2m"`); empty means `0`
  (immediate). An unparseable value is a validation error.

**Where `retry:` is allowed:** only on steps that execute a script — `run:`
steps, including `runsIn.image`/scope steps and `container:` steps. `retry:` on a
`call:`, `cache:`, `uploadArtifact:`/`downloadArtifact:`, or `approval:` step is a
validation error at apply time (call re-runs a whole child run — out of scope;
cache/artifact are already best-effort). Foreach/matrix and parallel steps may
carry `retry:` — each expanded instance retries independently.

### Wire

`api.ClaimStep` gains `Retry *RetrySpec` (`{Attempts int, Backoff string}`). The
controller's `buildOneClaimStep` copies `entry.Retry` through unchanged (it does
not interpret it — retry is executed entirely by the agent). Planned-step
synthesis is unaffected.

### Agent orchestrator: the retry loop

In `internal/agent/orchestrator.go`'s `makeStepRunner`, the block that executes a
`run:`/scope/`container:` step (currently one pass through
`RunInScope`/`RunNamedContainer`/`RunDefault` producing `ec`/`runErr`) is wrapped
in a retry loop:

```
attempts := 1; backoff := 0
if step.Retry != nil { attempts = step.Retry.Attempts; backoff = parse(step.Retry.Backoff) }
for try := 1; try <= attempts; try++ {
    run the step body → ec, runErr
    if success (runErr == nil && ec == 0) { status = Succeeded; break }
    if cancelledByMaster { status = Cancelled; break }   // never retry a master/user cancel
    if try < attempts {
        write a separator line to the step's stderr stream:
            "── retry <try+1>/<attempts> after <backoff> (previous: exit <ec>) ──"
        sleep backoff, honoring stepCtx cancellation (select on ctx.Done())
        continue
    }
    status = Failed   // last try failed
}
```

Semantics (matching the approved decisions):
- **What is retried:** any failure — a non-zero exit code, an exec/infra error
  (`runErr`), or a per-try timeout — is retryable. A master/user **cancellation
  is never retried** (the loop breaks and reports `Cancelled`).
- **Timeout is per-try:** the step's `timeoutMinutes` bounds *each* attempt (the
  existing per-step `stepCtx` timeout is created fresh for each try). A timed-out
  try counts as a failure and is retried (bounded by `attempts`).
- **`continueOnError` wraps the retry:** it applies only *after* all attempts are
  exhausted — if the step still fails after the last try, `continueOnError`
  decides whether the run fails (unchanged behavior, now fed the final result).
- **Only run/scope/container steps** reach this loop; the `call:` branch and
  cache/artifact steps are untouched.

### Logs & reporting

- **All attempts stream to the same step log** (one step, one log stream). A
  separator line on stderr marks each retry (`── retry 2/3 … ──`) so the boundary
  between attempts is visible; output is never discarded.
- **One `StepReport` per step**, as today. The final report carries the last
  attempt's `Status`/`ExitCode`. `api.StepReportRequest`/`StepReport` gain an
  optional `Attempts int` (the try number that finished, e.g. `2` when the 2nd
  try succeeded) so the UI can show "Succeeded (attempt 2/3)". Zero/omitted =
  no retry configured (rendered plainly). The intermediate "Running" report is
  unchanged (sent once at the start).
- Output-template capture (`{{ .Stdout }}`) and step outputs use the **final
  (successful) attempt's** output — the loop only captures outputs on success,
  exactly as the single-pass code does today.

### UI (optional, small)

`web/src/routes/RunDetail.svelte` renders the step's `attempts` next to its
status when present (e.g. a small `2/3` badge). Not required for the feature to
function (the separator lines already make retries visible in the log); include
it as a minor polish task.

## Error handling / edge cases

- `attempts: 1` (or no `retry:`) → exactly today's behavior; the loop runs once.
- Backoff sleep is `stepCtx`-aware: a master cancellation during a backoff wait
  aborts promptly and reports `Cancelled`, not `Failed`.
- A step that succeeds on try 1 pays zero overhead (no separator, no sleep).
- Retry does not change how a step's `post:` hook runs — the hook runs once after
  the step's final (success or exhausted-failure) result, as today.

## Testing

- **DSL** (`internal/dsl`): `retry.attempts >= 1` and duration parse validation;
  `retry:` on a non-run step (call/cache/artifact/approval) is a validation error;
  round-trip parse of a valid `retry` block.
- **Wire** (`internal/controller`): `buildOneClaimStep` copies `Retry` onto
  `api.ClaimStep` unchanged.
- **Agent orchestrator** (`internal/agent`): with a fake backend, a step that
  fails `N-1` times then succeeds reports `Succeeded` with `Attempts == N` and
  streams N attempts' logs with separators; a step that fails all `N` tries
  reports `Failed`; `continueOnError` after exhaustion does not fail the run; a
  per-try timeout is retried; a master cancellation mid-retry reports `Cancelled`
  and does not consume further attempts; `backoff` is honored (use a shrinkable
  test var / injected sleeper so tests stay fast).
- **UI**: if the attempts badge is added, a small render check; otherwise n/a.

## Non-goals

- Exponential backoff / jitter (fixed `backoff` only for the MVP; a `backoff`
  mode can be added later without breaking the schema).
- Conditional retry (`retryOn:` a specific exit code / pattern) — all failures
  are retried for now.
- Retrying `call:` steps (whole child runs), cache, or artifact steps.
- A run-level or job-level retry budget across steps (retry is per-step).
