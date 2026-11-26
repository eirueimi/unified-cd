# Run Metadata

Every step can find out which run, job, step and attempt it belongs to. The values are available in two places, both filled from the same source so they cannot disagree:

- as **environment variables** (`UNIFIED_RUN_*`, `UNIFIED_JOB_*`, `UNIFIED_STEP_*`), for shell steps and for SDKs that read the environment;
- as **template fields** (`.Run`, `.Job`, `.Step`, `.Agent`, and richer `.Steps.<name>`), for places that never pass through a shell, such as `uploadArtifact` paths and `call.with` values.

This works the same on host agents and the Kubernetes agent, for native and container steps. Nothing here carries a secret.

## Absent values are omitted

A value that is unknown is **absent**: the environment variable is not set at all, and the template field is the zero value (empty string, zero `time.Time`, empty map). It is never exported as an empty string. In a shell, test with `[ -n "${UNIFIED_RUN_URL:-}" ]`.

The one exception is `UNIFIED_RUN_PARAMS_JSON`, which is always set (`{}` when the run has no params).

## Run

| Env var | Template | Value | Argo Workflows equivalent |
|---|---|---|---|
| `UNIFIED_RUN_ID` | `.Run.ID` | Run UUID | `workflow.uid` |
| `UNIFIED_RUN_URL` | `.Run.URL` | `<externalUrl>/ui/#/runs/<id>`, with any trailing `/` on `externalUrl` trimmed. Absent when no external URL is configured (see [Configuration](../../reference/configuration.md) and the [Kubernetes integration](../../operator-manual/kubernetes-integration.md) guide for `controller.externalUrl`). | (none) |
| `UNIFIED_RUN_CREATED_AT` | `.Run.CreatedAt` | Env: RFC 3339 UTC. Template: `time.Time`, so `{{ .Run.CreatedAt.Format "2006-01-02" }}` works. | `workflow.creationTimestamp` |
| `UNIFIED_RUN_STARTED_AT` | `.Run.StartedAt` | Agent clock when the agent received the claim. Same types as `CreatedAt`. Set even with an old controller. | (none) |
| `UNIFIED_RUN_TRIGGERED_BY` | `.Run.TriggeredBy` | The raw `triggered_by` string, for example `webhook:gh-push` | (none) |
| `UNIFIED_RUN_TRIGGER` | `.Run.Trigger` | One of `api`, `webhook`, `schedule`, `call`, `replay` | (none) |
| `UNIFIED_RUN_SCHEDULED_AT` | `.Run.ScheduledAt` | Schedule runs only: the cron fire time the run was created for. Absent on a replay. | `workflow.scheduledTime` |
| `UNIFIED_RUN_PARENT_ID`, `UNIFIED_RUN_PARENT_JOB`, `UNIFIED_RUN_PARENT_STEP` | `.Run.Parent.ID`, `.Run.Parent.Job`, `.Run.Parent.Step` | `call:` children only. Absent on a replay of a child. | `workflow.mainEntrypoint` (closest match) |
| `UNIFIED_RUN_REPLAY_OF` | `.Run.ReplayOf` | Replays only: the original run ID | (none) |
| `UNIFIED_RUN_DISPLAY_NAME` | `.Run.DisplayName` | The resolved `displayName`. Absent when the job has none. | (none) |
| `UNIFIED_RUN_PARAMS_JSON` | `.Run.ParamsJSON` | All params as a JSON object. Always set. | `workflow.parameters.json` |
| `UNIFIED_RUN_AGENT_ID` | `.Agent.ID` | The claiming agent's ID | `steps.X.hostNodeName` (closest match) |

`UNIFIED_RUN_AGENT_ID` is deliberately not `UNIFIED_AGENT_ID`: that name is the agent's own configuration variable.

## Job

| Env var | Template | Value | Argo Workflows equivalent |
|---|---|---|---|
| `UNIFIED_JOB_NAME` | `.Job.Name` | Job name | `workflow.name` (closest match) |
| `UNIFIED_JOB_LABELS_JSON` | `.Job.Labels.<k>` | `metadata.labels` as recorded when the run was created. Absent when the job has no labels. | `workflow.labels.*` |
| `UNIFIED_JOB_ANNOTATIONS_JSON` | `.Job.Annotations.<k>` | `metadata.annotations` as recorded when the run was created. Absent when the job has no annotations. | `workflow.annotations.*` |

A run reports the labels and annotations the job had when the run started, not the job's current ones.

## Current step

| Env var | Template | Value | Argo Workflows equivalent |
|---|---|---|---|
| `UNIFIED_STEP_NAME` | `.Step.Name` | Step name (display name for matrix combinations) | `steps.name` |
| `UNIFIED_STEP_ATTEMPT` | `.Step.Attempt` | 1-based attempt number from the `retry` loop | `retries` (Argo counts from 0) |
| `UNIFIED_STEP_MAX_ATTEMPTS` | `.Step.MaxAttempts` | `retry.attempts`, or 1 | (none) |
| `UNIFIED_STEP_LAST_EXIT_CODE` | `.Step.LastExitCode` | From attempt 2 on: the previous attempt's exit code. Absent on attempt 1. | `lastRetry.exitCode` |

Each retry attempt expands its templates again, so `{{ .Step.Attempt }}` is current in every attempt.

`post:` hooks receive the run and job variables but **no** `UNIFIED_STEP_*` variables.

## Previous steps

`.Steps.<name>` carries `Status`, `ExitCode`, `StartedAt` and `EndedAt` in addition to `Outputs` and `ChildRunID`. These are template-only; they are not put into the environment.

| Field | Value |
|---|---|
| `.Steps.<name>.Status` | `Succeeded`, `Failed`, `Skipped` or `Cancelled` |
| `.Steps.<name>.ExitCode` | Exit code (0 for a skipped step) |
| `.Steps.<name>.StartedAt` | When the first attempt started (zero for a skipped step) |
| `.Steps.<name>.EndedAt` | When the last attempt ended (zero for a skipped step) |

An entry now exists after a step finishes in **any** status, not only after it succeeds. A template that used a missing entry to mean "did not succeed" must check `Status` instead. For matrix and foreach steps the times span all combinations (earliest start, latest end) and `Status` is the aggregate: `Failed` if any combination failed, otherwise `Succeeded`.

## `finally:` only

These exist only in `finally:` steps and are absent everywhere else.

| Env var | Template | Value | Argo Workflows equivalent |
|---|---|---|---|
| `UNIFIED_RUN_STATUS` | `.Run.Status` | `Succeeded`, `Failed` or `Cancelled`, as of when `finally:` began (the same view `failure()` reads) | `workflow.status` |
| `UNIFIED_RUN_FAILURES_JSON` | `.Run.Failures` | JSON array of the failed main-pipeline steps. Each entry has `name`, `stage`, `exitCode`, `startedAt`, `endedAt` and, for matrix combinations, `matrixKey`. `[]` when nothing failed. | `workflow.failures` |
| `UNIFIED_RUN_DURATION_SECONDS` | `.Run.Duration` | Whole seconds from `UNIFIED_RUN_STARTED_AT` to when `finally:` began | `workflow.duration` |

- A step that failed under `continueOnError: true` **is** listed in the failures, because it did fail, but it does not change `UNIFIED_RUN_STATUS`.
- Steps stopped by a run cancellation are **not** listed.
- `UNIFIED_RUN_FAILURES_JSON` is capped at 16384 bytes. When the list does not fit, trailing entries are dropped and the last array element is a marker object `{"truncated":N}`, where `N` is the number of entries omitted.

See [Approval and Finally](approval-and-finally.md) for the `finally:` block itself.

## Version skew

Mixed controller and agent versions keep working:

- **Old agent, new controller:** the extra claim fields are ignored and nothing changes for that agent.
- **New agent, old controller:** the controller sends no run metadata, so every value that depends on it (URL, created-at, trigger, triggered-by, scheduled-at, parent, replay-of, display name, labels, annotations) is absent. Values the agent knows itself are still set: run ID, job name, params, step name and attempt, agent ID, started-at, and the `finally:` status, failures and duration.

## Reserved names

The prefixes `UNIFIED_RUN_`, `UNIFIED_JOB_` and `UNIFIED_STEP_` are reserved (case-insensitive). A step `env:` key, a `Vars` manifest key or a `spec.vars` key that starts with one of them is rejected at apply time. See the [migration guide](../../operator-manual/migrations/2026-11-26-run-metadata-reserved-prefixes.md) for the exact error strings.

## Example

A step that posts a link to the run, and a `finally:` step that reports the outcome:
[`examples/jobs/run-metadata/`](https://github.com/eirueimi/unified-cd/tree/main/examples/jobs/run-metadata).
