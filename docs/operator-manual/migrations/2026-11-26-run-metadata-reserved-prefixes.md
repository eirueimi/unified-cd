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

## What breaks, and when

The rule is enforced in three places, so an unrenamed variable fails at three different moments:

1. **Apply time (controller upgraded).** `unified-cd apply` rejects the manifest with the strings above. Jobs
   already stored are not re-checked until they are applied again.
2. **AppSource sync (controller upgraded).** The reconciler re-applies every synced job on its next pass, so a
   job in a Git repository that still uses a reserved name fails to apply right after the controller upgrade. The
   file is reported with the same error as above (for `spec.vars`, wrapped as `spec.vars: invalid vars:
   "UNIFIED_JOB_X" is reserved`), and the previously stored definition stays in place until the repository is fixed.
3. **Run time (agent upgraded).** Stored jobs keep running, but an upgraded agent silently **drops** a stored
   `Vars` or `spec.vars` entry under the reserved prefixes: it is not put into the step environment and no error is
   shown, so a `run:` script that reads it breaks as soon as the agent is upgraded. A stored step `env:` key under a
   reserved prefix is not dropped, but it is overridden by the built-in value; the only trace is an agent log
   line, `step env overrides reserved run metadata; built-in value wins` (with `step` and `var` attributes).

## What to do

Rename before upgrading. Rename the variable to a name outside the three prefixes (`MY_RUN_ID`,
`BUILD_JOB_NAME`), update every `run:` script that reads it, and apply the job again (or merge the change for
AppSource-synced jobs) **before** you upgrade the controller and the agents. Because stored jobs are only checked
when applied, finding the names first is the safe order: grep your manifests and repositories for
`UNIFIED_RUN_`, `UNIFIED_JOB_` and `UNIFIED_STEP_`.

## Database migration 034

Migration 034 adds the run metadata columns and backfills `runs.trigger` with a single `UPDATE`
over every row of `runs`. On a large `runs` table this is a one-time write that holds row locks
and generates WAL for the duration of the statement. Run the upgrade in a quiet period, or
expire old runs first (`--run-retention-days` / `UNIFIED_RUN_RETENTION_DAYS`) to shorten it.

## Rolling upgrades and `UNIFIED_RUN_TRIGGER`

`runs.trigger` is nullable with no default. Controller replicas that still run the old version keep inserting
runs after migration 034 has been applied, and they do not write the column. Those rows stay NULL, which reads
back as an empty trigger, so `UNIFIED_RUN_TRIGGER` and `.Run.Trigger` are **absent** for them rather than
wrongly reported as `api`. Rows that existed when 034 ran are backfilled from the `triggered_by` prefix
(`webhook:`, `schedule:`, `replay:`, `agent:` map to `webhook`, `schedule`, `replay`, `call`; everything else
maps to `api`). Once every replica runs the new version, every new run has a trigger.
