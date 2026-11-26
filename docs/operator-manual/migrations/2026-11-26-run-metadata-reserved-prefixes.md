# Migrating: reserved `UNIFIED_RUN_`, `UNIFIED_JOB_` and `UNIFIED_STEP_` names

Every step now receives run metadata as environment variables (and as the template fields `.Run`,
`.Job`, `.Step` and `.Agent`). To keep those values from being overwritten, three name prefixes are
reserved: `UNIFIED_RUN_`, `UNIFIED_JOB_` and `UNIFIED_STEP_`. The check is case-insensitive.

| | Before | Now |
|---|---|---|
| Step `env:` key such as `UNIFIED_RUN_ID` | Accepted | Rejected at apply time (jobs, and the steps of a `kind: JobTemplate`, including steps a `uses:` expands) |
| `Vars` manifest key or `spec.vars` key such as `UNIFIED_JOB_X` | Accepted | Rejected at apply time |
| `UNIFIED_RUNNER`, `UNIFIED_RUNS` | Accepted | Still accepted: the rule matches the whole prefix, including the trailing underscore |

## Error strings

For a step (the same text for a `finally:` step, a `parallel:` sub-step and a step inside a
template):

```text
step "build": env "UNIFIED_RUN_ID" uses reserved prefix "UNIFIED_RUN_"
```

For a `Vars` manifest or `spec.vars`:

```text
"UNIFIED_JOB_X" is reserved
```

## What to do

Rename the variable to a name outside the three prefixes (`MY_RUN_ID`, `BUILD_JOB_NAME`) and update
every `run:` script that reads it. Jobs already stored are not re-checked until you apply them
again, so rename the variable in the manifest before the next apply, or the apply will fail.

## Database migration 034

Migration 034 adds the run metadata columns and backfills `runs.trigger` with a single `UPDATE`
over every row of `runs`. On a large `runs` table this is a one-time write that holds row locks
and generates WAL for the duration of the statement. Run the upgrade in a quiet period, or
expire old runs first (`--run-retention-days` / `UNIFIED_RUN_RETENTION_DAYS`) to shorten it.
