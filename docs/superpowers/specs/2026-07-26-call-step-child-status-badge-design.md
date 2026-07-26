# Call Step Child Status Badge Design

## Problem

The run detail API currently returns the status stored in `step_reports` for
every step. A `call:` step reports `Running` after it creates its child run,
then normally reports a terminal status after the child finishes. If the
parent agent disappears or the controller terminates the parent before that
final report, the parent step report remains `Running` even though the child
run is already `Failed` or `Cancelled`. The run detail UI renders that stale
value, so the badge beside the child-run link contradicts the linked run.

## Required Behavior

Once a call step has a valid `child_run_id`, the child run's current status is
the authoritative status returned for that step:

- `Pending`, `Queued`, `Running`, `Succeeded`, `Failed`, and `Cancelled` follow
  the child run as it changes.
- A step without a child run continues to use `step_reports.status`.
- If a recorded child run no longer exists, the API falls back to
  `step_reports.status`.
- Step timing and exit-code fields retain their existing meanings. This change
  only corrects the displayed status.

## Design

`Postgres.GetRunSteps` will left-join each step report to `runs` through
`step_reports.child_run_id`. The selected status will use the child run's
status when the join succeeds and otherwise use the stored step-report status.

This keeps status resolution in the data-access layer that already assembles
the step response. It requires no schema migration, API shape change, or
additional browser requests. The existing web UI will render the corrected
status without modification.

Synchronizing `step_reports` when a child changes state is intentionally
avoided. That approach would require every child termination and cancellation
path to update a second row and would still be vulnerable to missed updates.
Fetching each child from the browser is also avoided because it would add one
request per call step and duplicate backend status-resolution logic in the UI.

## Error Handling

The join is read-only and preserves the current query error behavior. A missing
child row produces `NULL` and falls back to the stored step status. Invalid
child identifiers are already prevented by the UUID column type.

## Tests

The store integration tests will prove that:

1. A call step whose stored status is `Running` returns the child run's current
   terminal status.
2. Changing the child run status is reflected without another step report.
3. A normal step without a child link still returns its stored status.

The regression will be verified with the focused store test and the repository
test suites appropriate to the touched Go package.

## Documentation

`docs/jobs.md` will state that the run detail status for a call step follows
the linked child run. No examples or templates change because the DSL and job
execution semantics remain unchanged.

## Out of Scope

- Rewriting historical `step_reports` rows.
- Changing call-step timing or exit-code presentation.
- Adding a separate child-status field or a second badge.
- Changing child-run cancellation behavior.
