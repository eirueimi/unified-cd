# Run Metadata for Job Steps — Design

Status: draft for review
Date: 2026-11-26
Companion: a separate design covers job-defined custom metrics. It uses the
metadata defined here as label values.

## Intent

A step cannot find out which run it belongs to today. Agents put three values
into a step's environment: `UNIFIED_AGENT_OS`, `UNIFIED_WORKSPACE` and
`UNIFIED_VOLUME_<ALIAS>`. Templates see `.Params`, `.Vars`, `.Steps`,
`.Secrets`, `.Foreach` and `.Matrix`, and nothing about the run.

Users want two things:

- build links back to the run, for example in a chat message, a PR comment or
  a deploy record;
- correlate their own metrics, logs and traces with a run, a job, a step and
  an attempt.

The target is parity with the runtime metadata that Argo Workflows exposes:
the global `workflow.*` values, per-step status and timing, retry counters,
and the exit-handler `workflow.status` and `workflow.failures`. Each item is
mapped to unified-cd concepts. Argo concepts that unified-cd does not have,
such as namespaces, service accounts and priority, are left out.

## Goals

- Every step gets run, job, step and attempt metadata as environment
  variables. This works the same on the host agent, the Kubernetes agent,
  native steps and container steps.
- The same values are available to templates as `.Run`, `.Job`, `.Step`
  and `.Agent`.
- `.Steps.<name>` also carries status, exit code and start and end times, so
  later steps and `finally:` can read them.
- `finally:` steps see the run status as of the moment `finally:` began, and
  the list of failed steps.
- A link to the run in the Web UI, when the controller knows its own
  browser-facing URL.

## Non-goals

- Job-defined custom metrics. They have their own design.
- Template support in places evaluated by the controller, such as
  `displayName` at trigger time and webhook mappings. Those keep their
  current contexts.
- A sequential per-job run number. Runs have only UUIDs today, and adding a
  number is a separate change.
- Per-combination status for matrix steps in `.Steps.<name>` (see Builder
  latitude).

## Anti-goals

- **Silent overriding.** An existing user variable must not silently replace
  a new built-in value, and a new built-in value must not silently replace a
  user variable. Collisions fail loudly at apply time.
- **Secrets in the new values.** Nothing added here may carry a secret.
- **Breaking mixed-version fleets.** A new agent talking to an old
  controller, or the reverse, must keep running jobs. Missing values are
  absent, never wrong.

## Approaches considered

1. **Environment variables only.** This covers shell steps and third-party
   SDKs, which read the environment. It cannot reach fields that never pass
   through a shell, such as `uploadArtifact` paths and `call:` params.
2. **Environment variables and templates (chosen).** Argo exposes its
   metadata as template variables, and parity needs the same reach. Both
   surfaces are filled from one per-step metadata value, so they cannot
   disagree.
3. **Templates only.** Rejected: metrics and tracing SDKs read the
   environment, not YAML.

The user picked approach 2 when they set Argo Workflows parity as the target.

## Metadata catalogue

All environment variable names are new. Every one of them starts with a
reserved prefix (see Reservation). A value that is unknown is **absent**: the
variable is not set, and the template field is the zero value. It is never
an empty string pretending to be a value.

### Run

| Env var | Template | Value | Argo equivalent |
|---|---|---|---|
| `UNIFIED_RUN_ID` | `.Run.ID` | run UUID | `workflow.uid` |
| `UNIFIED_RUN_URL` | `.Run.URL` | `<externalUrl>/ui/#/runs/<id>`, built by the controller with any trailing `/` on `externalUrl` trimmed. The route is `/runs/:id` in `web/src/App.svelte`, under the hash router served at `/ui/`. Absent if no external URL is configured. | (none) |
| `UNIFIED_RUN_CREATED_AT` | `.Run.CreatedAt` | Env: RFC 3339 UTC. Template: `time.Time`, so `.Run.CreatedAt.Format "2006-01-02"` works. | `workflow.creationTimestamp` (+ format) |
| `UNIFIED_RUN_STARTED_AT` | `.Run.StartedAt` | Agent clock when the agent received the claim. Same types as `CreatedAt`. Agent-known, so it is set even with an old controller. | (none) |
| `UNIFIED_RUN_TRIGGERED_BY` | `.Run.TriggeredBy` | The raw `triggered_by` string, for example `webhook:gh-push` | (none) |
| `UNIFIED_RUN_TRIGGER` | `.Run.Trigger` | One of `api`, `webhook`, `schedule`, `call`, `replay`, read from the new `runs.trigger` column (see Data model). It is never parsed from `triggered_by`: a principal's display name can look like `webhook:x`. | (none) |
| `UNIFIED_RUN_SCHEDULED_AT` | `.Run.ScheduledAt` | Schedule runs only: the cron fire time the run was created for. Absent on a replay, which is not a scheduled fire. | `workflow.scheduledTime` |
| `UNIFIED_RUN_PARENT_ID`, `UNIFIED_RUN_PARENT_JOB`, `UNIFIED_RUN_PARENT_STEP` | `.Run.Parent.ID`, `.Job`, `.Step` | `call:` children only, from `GetRunParent`. Absent on a replay of a child, which has no parent. | `workflow.mainEntrypoint` (closest match) |
| `UNIFIED_RUN_REPLAY_OF` | `.Run.ReplayOf` | Replays only: the original run ID | (none) |
| `UNIFIED_RUN_DISPLAY_NAME` | `.Run.DisplayName` | The resolved display name. Absent when the job has none. | (none) |
| `UNIFIED_RUN_PARAMS_JSON` | `.Run.ParamsJSON` | All params as a JSON object | `workflow.parameters.json` |
| `UNIFIED_RUN_AGENT_ID` | `.Agent.ID` | The claiming agent's ID | `steps.X.hostNodeName` (closest match) |

`UNIFIED_RUN_AGENT_ID` is deliberately not `UNIFIED_AGENT_ID`. That name is
already the agent's own configuration variable, and the step-environment
baseline filters it.

### Job

| Env var | Template | Value | Argo equivalent |
|---|---|---|---|
| `UNIFIED_JOB_NAME` | `.Job.Name` | job name | `workflow.name` (closest match) |
| `UNIFIED_JOB_LABELS_JSON` | `.Job.Labels.<k>` | `metadata.labels` as recorded at run creation | `workflow.labels.*` / `.json` |
| `UNIFIED_JOB_ANNOTATIONS_JSON` | `.Job.Annotations.<k>` | `metadata.annotations` as recorded at run creation | `workflow.annotations.*` / `.json` |

Labels and annotations are not sent to agents today, and they are not
persisted beyond the `path` annotation. This design records both on the run
at creation time (see Data model), so a run reports the job metadata it
started with, not today's.

### Current step

| Env var | Template | Value | Argo equivalent |
|---|---|---|---|
| `UNIFIED_STEP_NAME` | `.Step.Name` | step name (display name for matrix combinations) | `steps.name` / `node.name` |
| `UNIFIED_STEP_ATTEMPT` | `.Step.Attempt` | 1-based attempt number from the `retry` loop | `retries` (Argo counts from 0; this counts from 1) |
| `UNIFIED_STEP_MAX_ATTEMPTS` | `.Step.MaxAttempts` | `retry.attempts`, or 1 | (none) |
| `UNIFIED_STEP_LAST_EXIT_CODE` | `.Step.LastExitCode` | From attempt 2 on: the previous attempt's exit code | `lastRetry.exitCode` |

### Previous steps

`.Steps.<name>` gains `Status` (`Succeeded`, `Failed`, `Skipped`,
`Cancelled`), `ExitCode`, `StartedAt` and `EndedAt`. Today an entry exists
only after a step succeeds. With this change an entry exists after the step
finishes in any status, so `continueOnError` steps and `finally:` can see
failures. Code that tests for a missing entry to mean "not succeeded" has to
check `Status` instead. Find and fix every in-repo use.

Times for a step span all its attempts: `StartedAt` is when the first
attempt started and `EndedAt` when the last ended. A matrix or foreach step
spans its combinations: earliest start, latest end. A skipped step has
`Status: Skipped` and zero times and exit code.

These are template-only. Putting every earlier step into the environment of
each later step would make the environment grow with the job.

### `finally:` only

| Env var | Template | Value | Argo equivalent |
|---|---|---|---|
| `UNIFIED_RUN_STATUS` | `.Run.Status` | `Succeeded`, `Failed` or `Cancelled` as of when `finally:` began. This is the same frozen view that `failure()` reads. | `workflow.status` |
| `UNIFIED_RUN_FAILURES_JSON` | `.Run.Failures` | JSON array of the failed main-pipeline steps, each with `name`, `stage`, `exitCode`, `startedAt`, `endedAt` and `matrixKey` if set | `workflow.failures` |
| `UNIFIED_RUN_DURATION_SECONDS` | `.Run.Duration` | Seconds from `StartedAt` to when `finally:` began | `workflow.duration` |

The agent keeps a new failure list as each main-pipeline step finishes. It
has no such list today, only the `anyStepFailed` flag behind `statusView()`.
A step that failed under `continueOnError: true` **is** listed in
`.Run.Failures`, because it did fail, but it does not change
`.Run.Status`. That matches how `failure()` treats it today. Duration uses
the agent's `StartedAt`, so it is also set with an old controller.

Outside `finally:` these are absent. `UNIFIED_RUN_FAILURES_JSON` is capped at
16 KiB (Windows limits one variable to 32,767 characters). When the list does not fit, it is cut to the entries that fit and an
entry `{"truncated": <n omitted>}` is appended, so a long matrix cannot push
the environment past OS limits.

## Data model and protocol

- **Migration 034** adds four columns to `runs`:
  - `trigger text`, nullable with no default. Each `CreateRun` call site sets
    it explicitly (an empty origin trigger is written as `api`). Existing rows
    are backfilled once from the `triggered_by` prefix: `webhook:` becomes
    `webhook`, `schedule:` becomes `schedule`, `replay:` becomes `replay`,
    `agent:` becomes `call`, and anything else becomes `api`. The backfill is
    a one-time best guess for old rows only. During a rolling upgrade, old
    controller replicas keep inserting runs without the column; those rows
    stay NULL, which reads back as an empty trigger, so `UNIFIED_RUN_TRIGGER`
    and `.Run.Trigger` are absent for them rather than wrongly `api`.
  - `job_labels jsonb`, filled by `CreateRun` from the job's metadata at
    creation time;
  - `job_annotations jsonb`, the same;
  - `scheduled_at timestamptz NULL`, which the scheduler sets to the cron
    `next` time it fired for.

  Every `CreateRun` call site passes the job's metadata: trigger, webhook,
  schedule, `call:` child and replay. A replay copies the original run's labels
  and annotations. It leaves `scheduled_at` NULL and sets
  `trigger = 'replay'`. Labels already exist in the DSL as
  `metadata.labels` (`internal/dsl/types.go:19`). They are just not
  persisted. `api.Job` and job storage start persisting labels and annotations
  for this.
- **`ClaimResponse`** gains one `Run` object: `createdAt`, `triggeredBy`,
  `scheduledAt`, `parent` (from `GetRunParent`), `displayName`, `url`,
  `jobLabels` and `jobAnnotations`. `RunID`, `JobName` and `Params` are
  already there.
- **External URL.** Add a controller setting `externalUrl`
  (`UNIFIED_EXTERNAL_URL`, `--external-url`). It falls back to
  `oidc.externalUrl` and is otherwise unset. The controller never derives it
  from request headers or from the agent-facing URL, because those are often
  internal addresses.
- **Helm chart.** Add `controller.externalUrl` (default empty). The chart
  passes it to the controller as `UNIFIED_EXTERNAL_URL`. When it is empty,
  `ingress.enabled` is true and `ingress.hosts[0].host` is set, the chart
  derives the URL from that host. The scheme is `https` if any
  `ingress.tls[].hosts` entry contains that host, and `http` otherwise. An
  explicit `controller.externalUrl` always wins. Without either, the
  variable is not set. Document this in `charts/unified-cd/values.yaml` and
  the Kubernetes install docs, and cover the derivation with the chart's
  template tests or a `helm template` check in the Helm CI job.
- **Version skew.**
  - Old agent with a new controller: the new fields are ignored, and nothing
    changes for that agent.
  - New agent with an old controller: the `Run` object is missing, so every
    value that depends on it is absent. Values the agent knows itself are
    still set: run ID, job name, params, step, attempt, agent ID, status and
    failures.

## Reservation

Reserve the prefixes `UNIFIED_RUN_`, `UNIFIED_JOB_` and `UNIFIED_STEP_` in
`dsl.ReservedVarPrefixes`. Add each synthesised prefix to
`SynthesizedStepEnvPrefixes`. The existing tests then keep apply-time and
run-time rules in step.

Step-level `env:` that sets a reserved name is rejected at apply time with a
clear error. Today `step.Env` is not checked against reserved names. This
design adds that check for the new prefixes only. It runs on the job spec
after `uses:` templates are expanded, so template-provided `env:` is covered
too. A run whose snapshot was created before the upgrade can still carry such
a name. In that case the agent puts the built-in value last, so it wins, and
logs a warning naming the variable. Whether to extend the check to the
existing reserved names is left to the builder.

This is a **breaking change** for any `Vars` manifest or step `env:` that
already uses these prefixes. The release needs a migration guide in
`docs/operator-manual/migrations/` with the exact error strings.

## Where it is computed

The agent builds one per-step metadata value in `makeStepRunner`
(`internal/agent/orchestrator.go`), inside the retry loop so that `Attempt`
is current. From it come:

- the `extraEnv` entries, ahead of Vars and `step.Env`;
- the `.Run`, `.Job`, `.Step` and `.Agent` template fields.

The Kubernetes agent shares this loop through `agentlib.RunClaim`. Its scope
pods get the values through the same caller-expanded environment that
already overrides `imageStepEnv`. No second implementation is needed.

## Docs to update in the same change

- `docs/user-guide/writing-jobs/`: a new "Run metadata" page with the full
  catalogue, linked from the job-structure and templates pages.
- `docs/reference/configuration.md`: the `externalUrl` setting.
- `charts/unified-cd/values.yaml` and `docs/operator-manual/kubernetes-integration.md`: `controller.externalUrl` and the ingress-host fallback.
- `docs/reference/field-reference.md` and the schema: regenerate.
- `docs/operator-manual/migrations/`: the reserved prefixes, with the error
  text.
- `examples/`: one example that posts a link with `UNIFIED_RUN_URL` and
  reports `UNIFIED_RUN_FAILURES_JSON` from `finally:`.
- Fix the `UNIFIED_RUN_ID` caveats in `test/edgecase/scenarios/w1-5` and
  `w1-6`.

## Builder latitude

- Matrix and foreach steps: `.Steps.<name>.Status` is the aggregate. It is
  `Failed` if any combination failed, otherwise `Succeeded`. A per-combination
  view is out of scope.
- `.Run.Duration` type (seconds as a number, or `time.Duration`), as long as
  the env var holds integer seconds.
- Exact JSON field names in `UNIFIED_RUN_FAILURES_JSON`, if they stay
  camelCase and include the fields listed above.
- Whether `api.Job` exposes labels on the REST API. That is not needed here,
  but it is cheap to add while persisting them.
