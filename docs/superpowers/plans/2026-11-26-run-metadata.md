# Run Metadata for Job Steps Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every job step can read its run, job, step, attempt and agent
metadata from environment variables and from templates. `finally:` steps
also see the run status and the list of failed steps.

**Architecture:** The controller records the job's labels and annotations,
the trigger kind and the scheduled fire time on each run (migration 034). It
sends them to the agent in a new `ClaimResponse.Run` object, with a run URL
built from a new `externalUrl` setting. The agent's shared orchestrator
(`internal/agent/orchestrator.go`, which the Kubernetes agent also uses)
builds the per-step metadata. It turns that into `UNIFIED_RUN_*`,
`UNIFIED_JOB_*` and `UNIFIED_STEP_*` environment entries and into the new
template fields `.Run`, `.Job`, `.Step` and `.Agent`.

**Tech Stack:** Go, PostgreSQL migrations (`internal/store/migrations`),
Go `text/template`, Helm chart rendered in Go tests (`internal/helmchart`).

**Spec:** `docs/superpowers/specs/2026-11-26-run-metadata-design.md`

## Global Constraints

- All code, comments, docs and commit messages are in English. Never write
  PII. Use generic paths.
- Commit inside this worktree with
  `git -c user.name=eirueimi -c user.email="128969149+eirueimi@users.noreply.github.com" commit`.
  End every message with:
  - `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`
  - `Claude-Session: https://claude.ai/code/session_01G3NWwwXnjYtSfzJjZYTrnV`
- An unknown value is **absent**. The env var is not set at all, never set
  to `""`. The template field is its Go zero value.
- Reserved prefixes, exact strings: `UNIFIED_RUN_`, `UNIFIED_JOB_`,
  `UNIFIED_STEP_`.
- Trigger values, exact strings: `api`, `webhook`, `schedule`, `call`,
  `replay`.
- The run URL is `strings.TrimRight(externalURL, "/") + "/ui/#/runs/" + runID`.
- Times in env vars are RFC 3339 in UTC (`time.RFC3339`). Template time fields
  are `time.Time`.
- `UNIFIED_RUN_FAILURES_JSON` is at most 32768 bytes. Overflow is replaced by
  a trailing `{"truncated": N}` element.
- Do not hand-edit `docs/reference/field-reference.md` or
  `schemas/unified-cd.schema.json`. Run `go generate ./...`.
- Unit tests: `go test ./internal/<pkg>/ -run <Name>`. Store tests need a
  running Docker daemon (they start their own PostgreSQL).

## Review Focus

1. A replay of a scheduled or `call:` child run must not report
   `UNIFIED_RUN_SCHEDULED_AT` or `UNIFIED_RUN_PARENT_*` (Task 3 test
   `TestReplay_DropsScheduleAndParent`).
2. With a new agent and an old controller (`ClaimResponse.Run == nil`), the
   agent still sets the agent-known values and must not panic (Task 5 test
   `TestRunMetaEnv_NilClaimRun`).
3. A retried step sees `UNIFIED_STEP_ATTEMPT=2` and
   `UNIFIED_STEP_LAST_EXIT_CODE` on the second try, and the template
   `.Step.Attempt` agrees (Task 5 test `TestRetry_AttemptEnvAndTemplate`).
4. A step whose `env:` sets `UNIFIED_RUN_ID` is rejected at apply time, also
   when the `env:` comes from a `uses:` template (Task 2 test
   `TestParse_RejectsReservedStepEnv`, plus a `uses:` case).
5. `externalUrl` with a trailing slash must not produce `//ui/`, and an empty
   value gives no `UNIFIED_RUN_URL` (Task 3 test `TestClaimRunMeta_URL`).

---

### Task 1: Store — migration 034, run origin and job metadata

**Files:**
- Create: `internal/store/migrations/034_run_metadata.up.sql`, `034_run_metadata.down.sql`
- Modify: `internal/api/types.go` (`Job`, `Run`, new `ClaimRunMeta`, `ClaimResponse`, trigger constants)
- Modify: `internal/store/store.go:269,278`, `internal/store/postgres.go:271` and the claim and get queries that scan `runs`
- Modify: `internal/metrics/store.go:30` (wrapper signatures)
- Modify: every caller, so the tree builds. Pass zero values for now; Task 3 fills them.
- Test: `internal/store/postgres_runmeta_test.go`

**Interfaces:**
- Produces in `internal/api`:
  ```go
  const (TriggerAPI = "api"; TriggerWebhook = "webhook"; TriggerSchedule = "schedule"; TriggerCall = "call"; TriggerReplay = "replay")
  // Job gains:  Labels, Annotations map[string]string `json:"labels,omitempty"` / `json:"annotations,omitempty"`
  // Run gains:  Trigger string `json:"trigger,omitempty"`; ScheduledAt *time.Time `json:"scheduledAt,omitempty"`;
  //             JobLabels, JobAnnotations map[string]string `json:"jobLabels,omitempty"` / `json:"jobAnnotations,omitempty"`
  type ClaimRunMeta struct {
      CreatedAt      time.Time         `json:"createdAt"`
      TriggeredBy    string            `json:"triggeredBy"`
      Trigger        string            `json:"trigger"`
      ScheduledAt    *time.Time        `json:"scheduledAt,omitempty"`
      ReplayOf       string            `json:"replayOf,omitempty"`
      Parent         *CalledBy         `json:"parent,omitempty"`
      DisplayName    string            `json:"displayName,omitempty"`
      URL            string            `json:"url,omitempty"`
      JobLabels      map[string]string `json:"jobLabels,omitempty"`
      JobAnnotations map[string]string `json:"jobAnnotations,omitempty"`
  }
  // ClaimResponse gains: Run *ClaimRunMeta `json:"run,omitempty"`
  ```
- Produces in `internal/store`:
  ```go
  type RunOrigin struct { Trigger string; JobLabels, JobAnnotations map[string]string; ScheduledAt *time.Time }
  type JobMeta struct { Labels, Annotations map[string]string }
  CreateRun(ctx, jobName string, params map[string]string, spec []byte, agentSelector, requiredCaps []string, triggeredBy, displayName string, origin RunOrigin) (*api.Run, error)
  UpsertJob(ctx, name, apiVersion string, spec []byte, meta JobMeta) (*api.Job, error)
  ```
  `GetRun`, `GetJob`, `ClaimNextRun` and `ClaimNextRunDetached` return the
  new fields. `RunOrigin.Trigger == ""` is stored as `api`.

- [ ] **Step 1: Write the failing tests** in `postgres_runmeta_test.go`:
  - `TestCreateRun_StoresOrigin`: create with `RunOrigin{Trigger: "schedule", JobLabels: {"team":"a"}, JobAnnotations: {"x":"y"}, ScheduledAt: &t0}`. `GetRun` returns `Trigger == "schedule"`, the maps equal, and `ScheduledAt.Equal(t0)`.
  - `TestCreateRun_DefaultsTriggerAPI`: `RunOrigin{}` gives `Trigger == "api"`, nil maps and nil `ScheduledAt`.
  - `TestClaimNextRun_CarriesOrigin`: the claimed run carries the same four fields.
  - `TestUpsertJob_StoresLabelsAnnotations`: `GetJob` returns them, and a second upsert with nil maps clears them.
  - `TestMigration034_BackfillsTrigger`: insert rows with `triggered_by` values `webhook:a`, `schedule:b`, `replay:c`, `agent:d` and `alice`, using SQL against the pre-034 schema the way existing migration tests do. If none exist, insert after migrating and run the backfill `UPDATE` statement directly. Expect `webhook`, `schedule`, `replay`, `call` and `api`.
- [ ] **Step 2: Run** `go test ./internal/store/ -run 'CreateRun_|ClaimNextRun_CarriesOrigin|UpsertJob_Stores|Migration034'`. Expected: build failure or FAIL.
- [ ] **Step 3: Implement.**
  - The up migration adds:
    - to `jobs`: `labels jsonb`, `annotations jsonb`;
    - to `runs`: `trigger text NOT NULL DEFAULT 'api'`, `job_labels jsonb`, `job_annotations jsonb`, `scheduled_at timestamptz`.
  - It then backfills `runs.trigger` with one `UPDATE ... CASE` on the `triggered_by` prefixes, mapping `agent:` to `call`.
  - The down migration drops the six columns.
  - Update the interface, the Postgres implementation, the instrumented wrapper and all callers.
- [ ] **Step 4: Run** the Step 2 command, then `go build ./... && go vet ./internal/store/ ./internal/metrics/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(store): record run trigger, job metadata and scheduled time (migration 034)`.

### Task 2: DSL — template fields, step status data, reserved prefixes

**Files:**
- Modify: `internal/dsl/template.go:22-41` (`TemplateData`, `StepData`) and new types in the same file
- Modify: `internal/dsl/vars_parse.go:75` (`reservedVarPrefixes`)
- Modify: `internal/dsl/parse.go:83` (`Parse`). Also the place where `uses:` templates are expanded into a job's steps, if that is not inside `Parse`. Find it with `grep -rn "func.*[Ee]xpand.*[Uu]ses\|resolveUses" internal/`.
- Create: `docs/operator-manual/migrations/2026-11-26-run-metadata-reserved-prefixes.md`
- Test: `internal/dsl/template_runmeta_test.go`, `internal/dsl/parse_reserved_env_test.go`

**Interfaces:**
- Produces in `internal/dsl`:
  ```go
  type RunData struct {
      ID, URL, TriggeredBy, Trigger, ReplayOf, DisplayName, ParamsJSON, Status string
      CreatedAt, StartedAt, ScheduledAt time.Time
      Parent   ParentRunData
      Failures []StepFailure
      Duration int64 // seconds
  }
  type ParentRunData struct{ ID, Job, Step string }
  type JobData struct { Name string; Labels, Annotations map[string]string }
  type CurrentStepData struct { Name string; Attempt, MaxAttempts, LastExitCode int }
  type AgentData struct{ ID string }
  type StepFailure struct {
      Name      string    `json:"name"`
      Stage     int       `json:"stage"`
      ExitCode  int       `json:"exitCode"`
      StartedAt time.Time `json:"startedAt"`
      EndedAt   time.Time `json:"endedAt"`
      MatrixKey string    `json:"matrixKey,omitempty"`
  }
  // TemplateData gains: Run RunData; Job JobData; Step CurrentStepData; Agent AgentData
  // StepData gains:     Status string; ExitCode int; StartedAt, EndedAt time.Time
  const (RunEnvPrefix = "UNIFIED_RUN_"; JobEnvPrefix = "UNIFIED_JOB_"; StepEnvPrefix = "UNIFIED_STEP_")
  ```
  The error message for a reserved step env name, exact text:
  `step %q: env %q uses reserved prefix %q`, with arguments step name, env
  key and prefix.

- [ ] **Step 1: Write the failing tests.**
  - `TestExpandStepTemplate_RunFields`: `{{ .Run.ID }} {{ .Job.Labels.team }} {{ .Step.Attempt }} {{ .Agent.ID }} {{ .Run.CreatedAt.Format "2006-01-02" }}` expands to `r1 a 2 ag1 2026-11-26`.
  - `TestExpandStepTemplate_StepStatus`: `{{ .Steps.build.Status }}/{{ .Steps.build.ExitCode }}` expands to `Failed/3`.
  - `TestIsReservedVarName_NewPrefixes`: `UNIFIED_RUN_ID`, `unified_job_name` and `UNIFIED_STEP_X` are reserved. `UNIFIED_RUNNER` is not.
  - `TestParse_RejectsReservedStepEnv`: a Job with step `env: {UNIFIED_RUN_ID: x}` fails, and the error contains `uses reserved prefix "UNIFIED_RUN_"`. Cover a `finally:` step and a parallel sub-step too. Add a case where the `env:` comes from a `uses:` template, at whatever layer expands it.
  - Extend the existing `TestVarsParse`-style test so a `Vars` manifest key `UNIFIED_JOB_X` is rejected with the existing reserved-name error.
- [ ] **Step 2: Run** `go test ./internal/dsl/ -run 'RunFields|StepStatus|NewPrefixes|RejectsReservedStepEnv|Vars'`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - Add the types and fields.
  - Add the three prefixes to `reservedVarPrefixes`.
  - Add the step `env:` check, which uses `IsReservedVarName` restricted to the three new prefixes, and call it from `Parse` and after `uses:` expansion.
  - Write the migration guide. It needs a before/after table, the exact error strings for steps and for Vars, and the fix: rename the variable.
- [ ] **Step 4: Run** the Step 2 command, then `go test ./internal/dsl/` (includes `TestExamplesParse` and `TestTemplatesParse`). Expected: PASS.
- [ ] **Step 5: Commit** `feat(dsl): run metadata template fields and reserved UNIFIED_RUN_/JOB_/STEP_ prefixes`.

### Task 3: Controller — origins at every run creation, claim metadata, externalUrl

**Files:**
- Modify: `internal/controller/api_runs.go:43,55,93,174` (trigger, `createRunFromJob`, replay)
- Modify: `internal/controller/api_agent.go:589` (`buildClaimResponse`) and its caller near `:412`. Also `:1348` (`call:` child)
- Modify: `internal/controller/api_webhooks.go:288`, `internal/controller/scheduler.go:237`
- Modify: `internal/controller/api_jobs.go:78`, `internal/controller/appsource_apply.go:62` (pass `JobMeta`)
- Modify: `internal/config/controller.go` (new `ExternalURL`, following the `Addr` pattern for YAML, env and flag) and `cmd/controller/main.go` (wire it into the server)
- Test: `internal/controller/api_runmeta_test.go`, `internal/config/controller_externalurl_test.go`

**Interfaces:**
- Consumes: Task 1 `store.RunOrigin`, `store.JobMeta`, `api.ClaimRunMeta`, the trigger constants.
- Produces:
  - `func (s *Server) claimRunMeta(ctx context.Context, run api.Run) *api.ClaimRunMeta`. The claim handler sets `resp.Run`. A `GetRunParent` error is logged at warn and leaves `Parent` nil.
  - `createRunFromJob(ctx, jobName string, reqParams map[string]string, triggeredBy, trigger string)`.
  - Config: YAML `externalUrl`, env `UNIFIED_EXTERNAL_URL`, flag `--external-url`. The effective value falls back to `oidc.externalUrl`.

- [ ] **Step 1: Write the failing tests.**
  - `TestCreateRun_TriggerPerSource`: manual trigger gives `api`, webhook `webhook`, schedule `schedule` with `ScheduledAt` equal to the cron fire time the scheduler computed, `call:` child `call`, replay `replay`. Assert through `GetRun`.
  - `TestCreateRun_CopiesJobLabels`: after applying a job with `metadata.labels` and `annotations`, a new run carries them.
  - `TestReplay_DropsScheduleAndParent`: replaying a schedule run gives nil `ScheduledAt`, the labels copied from the original, and `Trigger == "replay"`. Its claim metadata has `ReplayOf == <orig id>` and nil `Parent`.
  - `TestClaimRunMeta_Parent`: a `call:` child's claim has `Parent` with the parent ID, job and step.
  - `TestClaimRunMeta_URL`: with `externalUrl` `https://cd.example.com/` the URL is `https://cd.example.com/ui/#/runs/<id>`. With an empty value, `URL == ""`.
  - `TestControllerConfig_ExternalURLFallback`: YAML, env and flag precedence follow the existing `Addr` tests. With it unset and `oidc.externalUrl` set, the effective value is the OIDC one.
- [ ] **Step 2: Run** `go test ./internal/controller/ ./internal/config/ -run 'TriggerPerSource|CopiesJobLabels|Replay_Drops|ClaimRunMeta|ExternalURL'`. Expected: FAIL.
- [ ] **Step 3: Implement.** Use the scheduler's computed fire time (`next` in `checkAndFireSchedules`) for `ScheduledAt`. Derive `ReplayOf` from `triggered_by` only when `Trigger == "replay"`.
- [ ] **Step 4: Run** the Step 2 command, then `go test ./internal/controller/ ./internal/config/`. Expected: PASS.
- [ ] **Step 5: Docs in the same task:** add `externalUrl`, `UNIFIED_EXTERNAL_URL` and `--external-url` to `docs/reference/configuration.md`, next to the OIDC `externalUrl` entry, and say that it falls back to the OIDC value.
- [ ] **Step 6: Commit** `feat(controller): send run metadata on claim; add externalUrl`.

### Task 4: Helm — controller.externalUrl with ingress fallback

**Files:**
- Modify: `charts/unified-cd/values.yaml` (`controller.externalUrl: ""` with a comment), `charts/unified-cd/values.schema.json`
- Modify: `charts/unified-cd/templates/_helpers.tpl` (`unified-cd.controllerConfigDefault` emits `externalUrl:`, and a new `define "unified-cd.externalUrl"`)
- Modify: `docs/operator-manual/kubernetes-integration.md` and `charts/unified-cd/README.md`
- Test: `internal/helmchart/controller_test.go`

**Interfaces:**
- Consumes: the Task 3 YAML key `externalUrl`.
- Produces: the rendered controller config contains `externalUrl: <value>` when a value resolves, and no key when it does not. `controller.config.externalUrl` still overrides it through the existing deep merge.

- [ ] **Step 1: Write the failing tests**, using the existing render helpers in `internal/helmchart/render.go`:
  - `TestControllerExternalURL_Explicit`: `controller.externalUrl=https://x.example` renders `externalUrl: https://x.example`, even when an ingress is set.
  - `TestControllerExternalURL_FromIngressTLS`: ingress enabled, `hosts[0].host=cd.example.com`, `tls[0].hosts=[cd.example.com]` renders `https://cd.example.com`.
  - `TestControllerExternalURL_FromIngressNoTLS` renders `http://cd.example.com`.
  - `TestControllerExternalURL_Unset`: with no ingress and no value, the `externalUrl` key is absent.
- [ ] **Step 2: Run** `go test ./internal/helmchart/ -run ExternalURL`. Expected: FAIL.
- [ ] **Step 3: Implement** the helper and the values. Then run `bash scripts/build-manifests.sh` if the bundles under `charts/unified-cd/bundles` are generated from the chart and checked in. Check the script header for this.
- [ ] **Step 4: Run** `go test ./internal/helmchart/` and `helm lint --strict charts/unified-cd`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(chart): controller.externalUrl with ingress-host fallback`.

### Task 5: Agent — build metadata, env, templates, step results, failures

**Files:**
- Create: `internal/agent/runmeta.go`
- Modify: `internal/agent/orchestrator.go`:
  - `:212` `RunClaim`: record `startedAt` and build the base metadata;
  - `:292` initial `TemplateData`;
  - `:558` `makeStepRunner`;
  - `:797-860`: move `run:` and `env:` template expansion inside the retry loop at `:859`, so each attempt sees the current `.Step`;
  - `:997` and the failure and skip paths: record step results;
  - `:1242`: frozen finally status.
- Modify: `internal/agent/pipeline.go` (`safeStepCtx`: a new `setStepResult` and a failure list)
- Modify: `internal/agent/stepenv.go:142` (`SynthesizedStepEnvPrefixes` adds the three prefixes)
- Test: `internal/agent/runmeta_test.go` and orchestrator tests next to the existing ones, using `orchestrator_vars_test.go` as the pattern

**Interfaces:**
- Consumes: Task 1 `api.ClaimRunMeta` and `ClaimResponse.Run`. Task 2 types and prefixes.
- Produces (package-internal):
  ```go
  type runMeta struct{ /* run-level values */ }
  func newRunMeta(c api.ClaimResponse, agentID string, startedAt time.Time) runMeta
  func (m runMeta) templateData(step dsl.CurrentStepData, final *finalView) (dsl.RunData, dsl.JobData, dsl.AgentData)
  func (m runMeta) env(step dsl.CurrentStepData, final *finalView) []string // "K=V" entries, absent values omitted
  type finalView struct { Status string; Failures []dsl.StepFailure; Duration time.Duration }
  func failuresJSON(f []dsl.StepFailure, max int) string // max = 32768
  func (c *safeStepCtx) setStepResult(name string, r dsl.StepData)
  func (c *safeStepCtx) addFailure(f dsl.StepFailure)
  func (c *safeStepCtx) failures() []dsl.StepFailure
  ```
  The env names come from the spec's catalogue: `UNIFIED_RUN_ID`, `_URL`,
  `_CREATED_AT`, `_STARTED_AT`, `_TRIGGERED_BY`, `_TRIGGER`,
  `_SCHEDULED_AT`, `_PARENT_ID`, `_PARENT_JOB`, `_PARENT_STEP`,
  `_REPLAY_OF`, `_DISPLAY_NAME`, `_PARAMS_JSON`, `_AGENT_ID`, `_STATUS`,
  `_FAILURES_JSON`, `_DURATION_SECONDS`; `UNIFIED_JOB_NAME`,
  `_LABELS_JSON`, `_ANNOTATIONS_JSON`; `UNIFIED_STEP_NAME`, `_ATTEMPT`,
  `_MAX_ATTEMPTS`, `_LAST_EXIT_CODE`. Labels and annotations JSON is
  omitted when the map is empty.

  Env order: the metadata entries are appended **after** `step.Env`, so a
  built-in wins. When a `step.Env` key has a reserved prefix, which can only
  happen in a pre-upgrade run snapshot, the agent logs
  `slog.Warn("step env overrides reserved run metadata; built-in value wins", "step", ..., "var", ...)`.

  Post hooks (`:1186`) get the run-level and job-level entries, without
  `UNIFIED_STEP_*`.

  `.Steps.<name>` results: on finish in any status, call `setStepResult`.
  Skipped steps get `Status: "Skipped"`. Matrix and foreach steps
  aggregate: `Failed` if any combination failed, earliest `StartedAt`,
  latest `EndedAt`. Keep `setStep`'s existing outputs behaviour.

  Failure list: add every main-pipeline step that ends `Failed`, including
  `continueOnError` ones, with `Stage = step.StageIndex` and
  `MatrixKey = step.MatrixKey`. `finally:` steps are never added.

  Finally view: built at `:1242` from the frozen status. `Status` is
  `Cancelled` if `cancelled`, `Failed` if `mainFailed`, otherwise
  `Succeeded`. `Duration` is `time.Since(startedAt)`, and the env var holds
  integer seconds. The main-pipeline runner passes `final == nil`.

- [ ] **Step 1: Write the failing tests.**
  - `TestRunMetaEnv_Full`: given a `ClaimResponse` with every `Run` field, `env` contains exactly the expected `K=V` set, with RFC 3339 UTC times and compact `UNIFIED_JOB_LABELS_JSON`.
  - `TestRunMetaEnv_NilClaimRun`: with `c.Run == nil`, the env still has `UNIFIED_RUN_ID`, `_STARTED_AT`, `_PARAMS_JSON`, `_AGENT_ID`, `UNIFIED_JOB_NAME` and the step entries. `_URL`, `_TRIGGER` and `_CREATED_AT` are absent. No panic.
  - `TestRunMetaEnv_AbsentNotEmpty`: no entry ends in `=`. For example, no `UNIFIED_RUN_URL=` when the URL is empty.
  - `TestFailuresJSON_Truncates`: 5000 failures produce output of at most 32768 bytes that ends with `{"truncated":N}`, where N plus the kept entries equals 5000.
  - `TestRetry_AttemptEnvAndTemplate`: a step with `retry.attempts: 2` fails on try 1 with exit 7. On try 2 its env has `UNIFIED_STEP_ATTEMPT=2`, `UNIFIED_STEP_MAX_ATTEMPTS=2` and `UNIFIED_STEP_LAST_EXIT_CODE=7`, and `run: echo {{ .Step.Attempt }}` expanded to `echo 2`.
  - `TestStepsStatus_VisibleAfterFailure`: a `continueOnError` failing step `a`, followed by step `b` whose `run` uses `{{ .Steps.a.Status }}`, gives `Failed`.
  - `TestFinally_StatusAndFailures`: a failing main step makes the `finally:` step's env contain `UNIFIED_RUN_STATUS=Failed`, a `UNIFIED_RUN_FAILURES_JSON` naming that step, and `UNIFIED_RUN_DURATION_SECONDS`. A main step does not get `UNIFIED_RUN_STATUS`.
  - `TestStepEnv_BuiltinWinsOverReservedStepEnv`: a claim whose step has `env: {UNIFIED_RUN_ID: x}` runs with the real ID, and the warning is logged.
  - The existing `TestSynthesizedStepEnvPrefixes_AreReservedAtApplyTime` must still pass.
- [ ] **Step 2: Run** `go test ./internal/agent/ -run 'RunMeta|FailuresJSON|Retry_Attempt|StepsStatus|Finally_Status|BuiltinWins|Synthesized'`. Expected: FAIL.
- [ ] **Step 3: Implement** as specified in Interfaces. Grep the agent and dsl packages for code that treats a missing `Steps[name]` as "not succeeded" (`grep -rn "Steps\[" internal/agent internal/dsl`), and switch it to `Status`.
- [ ] **Step 4: Run** `go test ./internal/agent/ ./internal/k8sagent/ ./internal/dsl/`. Expected: PASS. On Windows the agent tests are hermetic (`hermetic_test.go`). Do not add tests that need a real container runtime outside `//go:build integration`.
- [ ] **Step 5: Commit** `feat(agent): expose run metadata to steps via env and templates`.

### Task 6: User docs, examples, generated artifacts

**Files:**
- Create: `docs/user-guide/writing-jobs/run-metadata.md` (the full catalogue table from the spec, the absent-value rule, the finally-only values, version skew, and the reserved prefixes)
- Modify: `docs/user-guide/writing-jobs/job-structure.md` and the templates or expressions page under `docs/user-guide/writing-jobs/` (find it with `grep -ln "{{ .Params" docs/user-guide/writing-jobs`): link the new page and list `.Run`, `.Job`, `.Step`, `.Agent` and the new `.Steps.<name>` fields
- Modify: the docs site nav, if pages are listed explicitly (check `mkdocs.yml` or the site config at the repo root)
- Create: `examples/run-metadata/job.yaml` and a README: a step that echoes a link with `$UNIFIED_RUN_URL`, and a `finally:` step that prints `$UNIFIED_RUN_STATUS` and `$UNIFIED_RUN_FAILURES_JSON`
- Modify: `test/edgecase/scenarios/w1-5-oneway-partition-zombie.md:33` and `w1-6-credential-revocation.md:26`: replace the "`UNIFIED_RUN_ID` is not injected" caveat
- Modify: `README.md` feature summary (one line)
- Regenerate: `go generate ./...`

- [ ] **Step 1:** Write the docs and the example. Every env name and template field must match Task 5's list exactly.
- [ ] **Step 2: Run** `go generate ./... && git status --short` (any generated diff gets committed), then `go test ./internal/dsl/ -run 'Examples|Templates'`. Expected: PASS.
- [ ] **Step 3: Run** `grep -rn "UNIFIED_RUN_ID\` is \*\*not\*\*\|is not injected" docs test examples`. Expected: no hits.
- [ ] **Step 4: Commit** `docs: run metadata reference, example and configuration`.
