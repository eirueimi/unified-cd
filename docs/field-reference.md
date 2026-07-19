# unified-cd Field Reference

> This file is auto-generated. Do not edit it directly.
> Regenerate with `go generate ./internal/dsl/`.

## Table of Contents

- [Job](#job)
- [JobTemplate](#jobtemplate)
- [Schedule](#schedule)
- [WebhookReceiver](#webhookreceiver)
- [AppSource](#appsource)
- [GitCredential](#gitcredential)

---

## Job

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `apiVersion` | string | yes |  |
| `kind` | string | yes |  |
| `metadata` | Metadata | yes |  |
| `spec` | Spec | yes |  |

### Metadata

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `annotations` | map[string]string | no |  |
| `labels` | map[string]string | no |  |
| `name` | string | yes |  |

### Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `agentSelector` | []string | no |  |
| `concurrency` | Concurrency | no |  |
| `description` | string | no | Description is a human-readable summary of the job, shown in the WebUI. |
| `finally` | []StepEntry | no | Finally runs after the main DAG completes, on success, failure, or
cancellation. Same structure as Steps. A finally step's `if:` defaults to
always-run; use if: failure()/success() to filter. A finally step that
fails marks the run Failed (after all finally steps run). |
| `native` | boolean | no | Native opts the whole job into host-process execution (no claim pod,
no podTemplate, no container: steps). Host agents only; the default
(false) is the isolated pod model on both backends. |
| `params` | Params | yes |  |
| `podTemplate` | PodTemplate | no |  |
| `shell` | []string | no | Shell overrides the default interpreter argv for every step in this
job that does not declare its own step-level shell:. Array-only (no
scalar shorthand); the run: script is appended as the final argv
element. See Step.Shell for the full resolution priority. |
| `steps` | []StepEntry | yes | Steps is the main DAG of steps to execute.
(failFast was removed — all started steps run to completion.) |
| `timeoutMinutes` | number | no |  |

### Concurrency

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `mutex` | string | no |  |
| `orLocks` | []OrLock | no |  |
| `semaphores` | []Semaphore | no |  |

### OrLock

OrLock acquires exactly one candidate from In — whichever is free — instead of
requiring all of them like Semaphores does. The acquired candidate value is
exposed to the Job's steps as a synthesized parameter named
strings.ToUpper(Name)+"_LOCK_VALUE" (e.g. Name "env" -> "ENV_LOCK_VALUE"),
readable via {{ .Params.ENV_LOCK_VALUE }}.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `in` | ForeachSource | yes |  |
| `name` | string | yes |  |

### ForeachSource

ForeachSource is either a literal list (YAML sequence) or a template expression (YAML string).

  in: [prod, staging, dev]                    → Literal
  in: $envs                                   → Expr (JSON-array param reference)
  in: "{{ .Params.envs | split \",\" }}"      → Expr (template)
  in: "{{ .Steps.list.Outputs.envs | split \",\" }}" → Expr (step output reference)

### Semaphore

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `capacity` | integer | yes |  |
| `pool` | string | yes |  |

### StepEntry

StepEntry is either a concrete step (Name is set) or a parallel group (Parallel is set).
The two forms are mutually exclusive; Validate enforces this.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `approval` | ApprovalStep | no |  |
| `cache` | CacheStep | no |  |
| `call` | CallStep | no |  |
| `container` | string | no |  |
| `continueOnError` | boolean | no |  |
| `downloadArtifact` | DownloadArtifactStep | no |  |
| `env` | map[string]string | no |  |
| `foreach` | ForeachDef | no |  |
| `if` | string | no |  |
| `matrix` | MatrixDef | no |  |
| `name` | string | no | Concrete step fields (identical to Step, minus Needs) |
| `outputs` | map[string]string | no |  |
| `parallel` | []Step | no | Parallel group (mutually exclusive with all concrete step fields above) |
| `post` | PostStep | no |  |
| `retry` | RetrySpec | no |  |
| `run` | string | no |  |
| `runsIn` | RunsIn | no |  |
| `scopeID` | string | no | Scope tagging: set by inline expansion when a uses-level runsIn.image
makes the whole template one isolated scope. Steps sharing ScopeID run
in one environment. Not user-authored. |
| `scopeImage` | string | no |  |
| `shell` | []string | no | Shell overrides the effective interpreter argv for this step. See
Step.Shell for the full resolution priority. |
| `timeoutMinutes` | number | no |  |
| `uploadArtifact` | UploadArtifactStep | no |  |
| `uses` | UsesStep | no |  |

### ApprovalStep

ApprovalStep pauses the run until an authenticated user approves or rejects.
TimeoutMinutes defaults to 60 (applied at compile time) when zero.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `message` | string | no |  |
| `timeoutMinutes` | number | no |  |

### CacheStep

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `key` | string | yes | cache key; supports template expansion |
| `path` | string | yes | directory to cache; supports template expansion |
| `restoreKeys` | []string | no | fallback key prefixes; support template expansion |
| `ttlDays` | integer | no | default 30, max 365 |

### CallStep

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `job` | string | yes |  |
| `with` | map[string]any | no |  |

### DownloadArtifactStep

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `destDir` | string | no | defaults to the current directory if omitted |
| `name` | string | yes |  |

### ForeachDef

ForeachDef expands a step into one parallel run per item in the list.
Key is the variable name accessible in templates as {{ .Foreach.key }}.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `in` | ForeachSource | yes |  |
| `key` | string | yes |  |

### MatrixDef

MatrixDef expands a step into one copy per combination of dimension values
(cartesian product minus exclude entries). Dimensions preserve YAML
declaration order; the combination key joins values with "/" in that order
(e.g. "linux/amd64").

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `exclude` | []map[string]string | no |  |

### Step

Step is a concrete step. Used inside parallel: blocks and as the body of a StepEntry.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `approval` | ApprovalStep | no |  |
| `cache` | CacheStep | no |  |
| `call` | CallStep | no |  |
| `container` | string | no |  |
| `continueOnError` | boolean | no |  |
| `downloadArtifact` | DownloadArtifactStep | no |  |
| `env` | map[string]string | no |  |
| `foreach` | ForeachDef | no |  |
| `if` | string | no |  |
| `matrix` | MatrixDef | no |  |
| `name` | string | yes |  |
| `outputs` | map[string]string | no |  |
| `post` | PostStep | no |  |
| `retry` | RetrySpec | no |  |
| `run` | string | no |  |
| `runsIn` | RunsIn | no |  |
| `scopeID` | string | no | Scope tagging: set by inline expansion when a uses-level runsIn.image
makes the whole template one isolated scope. Steps sharing ScopeID run
in one environment. Not user-authored. |
| `scopeImage` | string | no |  |
| `shell` | []string | no | Shell overrides the effective interpreter argv for this step. Array
form only (v1): e.g. [bash, -lc] or [python3, -c]; the run: script is
appended as the final argv element. Resolution priority (most specific
wins): step.shell > a uses: template's own declared shell > spec.shell
(job-level) > system default. Steps inside parallel: and finally:
count as steps for this purpose. |
| `timeoutMinutes` | number | no |  |
| `uploadArtifact` | UploadArtifactStep | no |  |
| `uses` | UsesStep | no |  |

### PostStep

PostStep defines cleanup/post-processing to run after a step completes.
Executed in LIFO order after RunDAG completes.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `env` | map[string]string | no |  |
| `run` | string | no |  |
| `shell` | []string | no | Shell overrides the interpreter argv for this post hook. When absent,
the hook inherits its owning step's effective shell. The override
exists because inheritance alone breaks down for non-shell
interpreters: a step running under shell: [python3, -c] with a
shell-script cleanup hook needs post: {shell: [sh, -c], run: ...} to
be expressible at all. |

### RetrySpec

RetrySpec configures automatic re-runs of a failing run: step.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `attempts` | integer | yes | Attempts is the total number of tries (1 = no retry). Must be >= 1. |
| `backoff` | string | no | Backoff is a fixed wait between tries as a Go duration (e.g. "30s").
Empty means 0 (immediate retry). |

### RunsIn

RunsIn declares the execution context for a uses: template entry. It is no
longer legal on a plain step (step-level runsIn: was removed; the flat
container: field is the canonical way to pin a plain step to a podTemplate
container). On a uses: entry, only the image form is accepted: it declares
that the whole inlined template runs in one fresh isolated scope built from
this image (host: `<rt> run`; k8s: a throwaway pod). No workspace is shared
— pass inputs via with:/env, return outputs via outputs:/stdout.
runsIn.container on a uses: entry is rejected; set container: on the
template's own steps instead.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `container` | string | no |  |
| `image` | string | no |  |
| `resources` | ResourceSpec | no |  |

### ResourceSpec

ResourceSpec declares CPU/memory requests and limits for a runsIn.image step.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `limits` | ResourceList | no |  |
| `requests` | ResourceList | no |  |

### ResourceList

ResourceList is a cpu/memory pair using Kubernetes quantity strings
(e.g. "500m", "1", "256Mi", "1Gi").

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `cpu` | string | no |  |
| `memory` | string | no |  |

### UploadArtifactStep

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes |  |
| `path` | string | yes |  |

### UsesStep

UsesStep inlines a git-template job's steps directly into the current run.
Job must be a git:// URI; unlike CallStep, it never references a registered job name.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `job` | string | yes |  |
| `with` | map[string]any | no |  |

### Params

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `inputs` | []Input | no |  |
| `outputs` | []Output | no |  |

### Input

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `default` | any | no |  |
| `description` | string | no |  |
| `name` | string | yes |  |
| `pattern` | string | no | Pattern is a regular expression every supplied value must match (defaults
are checked too, so a bad default cannot slip through). Param values are
interpolated into step shell text, so a param fed from an untrusted
source — a webhook payload especially — is a command-injection vector
unless constrained. Suggested starting point: ^[A-Za-z0-9._/-]+$ |
| `required` | boolean | no |  |
| `type` | `string` \| `bool` \| `int` \| `array` | yes |  |
| `unvalidated` | boolean | no | Unvalidated explicitly opts this input out of the pattern requirement for
payload-mapped params. Use only when the value is genuinely free-form and
never reaches a shell. |

### Output

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes |  |
| `type` | `string` \| `bool` \| `int` \| `artifact` | yes | "string", "bool", "int", "artifact" |

### PodTemplate

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `cleanWorkspace` | boolean | no |  |
| `name` | string | no |  |
| `override` | PodSpecPatch | no |  |
| `reuse` | boolean | no |  |
| `spec` | map[string]any | no |  |
| `workspace` | WorkspaceConfig | no |  |

### PodSpecPatch

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `containers` | []map[string]any | no |  |
| `volumes` | []map[string]any | no |  |

### WorkspaceConfig

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `mountPath` | string | no |  |
| `pvc` | WorkspacePVC | no |  |

### WorkspacePVC

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `accessMode` | `ReadWriteOnce` \| `ReadOnlyMany` \| `ReadWriteMany` | no |  |
| `claimName` | string | no |  |
| `storageClassName` | string | no |  |
| `storageRequest` | string | no |  |

## JobTemplate

JobTemplate is the resource a uses: step points at. Unlike a full Job, its
schema contains ONLY what uses: can honor — the template's steps are inlined
into the CALLER's run and pod, so fields that would shape a different pod,
agent, or run (agentSelector, concurrency, timeoutMinutes, native,
podTemplate reuse/workspace/override, pod-level spec keys) do not exist here
and are rejected by strict decoding. A job that needs its own pod/agent/run
semantics should be invoked with call: instead.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `apiVersion` | string | yes |  |
| `kind` | string | yes |  |
| `metadata` | Metadata | yes |  |
| `spec` | JobTemplateSpec | yes |  |

### Metadata

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `annotations` | map[string]string | no |  |
| `labels` | map[string]string | no |  |
| `name` | string | yes |  |

### JobTemplateSpec

JobTemplateSpec is the uses:-supported subset of a job spec.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `description` | string | no |  |
| `finally` | []StepEntry | no | Finally steps run in the CALLER's finally phase (appended after the
caller's own finally steps, prefixed like all inlined steps). Rejected
in scope mode (runsIn.image), where the scope pod's lifetime ends with
the template body. |
| `params` | Params | no |  |
| `podTemplate` | JobTemplatePodTemplate | no |  |
| `shell` | []string | no |  |
| `steps` | []StepEntry | yes |  |

### StepEntry

StepEntry is either a concrete step (Name is set) or a parallel group (Parallel is set).
The two forms are mutually exclusive; Validate enforces this.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `approval` | ApprovalStep | no |  |
| `cache` | CacheStep | no |  |
| `call` | CallStep | no |  |
| `container` | string | no |  |
| `continueOnError` | boolean | no |  |
| `downloadArtifact` | DownloadArtifactStep | no |  |
| `env` | map[string]string | no |  |
| `foreach` | ForeachDef | no |  |
| `if` | string | no |  |
| `matrix` | MatrixDef | no |  |
| `name` | string | no | Concrete step fields (identical to Step, minus Needs) |
| `outputs` | map[string]string | no |  |
| `parallel` | []Step | no | Parallel group (mutually exclusive with all concrete step fields above) |
| `post` | PostStep | no |  |
| `retry` | RetrySpec | no |  |
| `run` | string | no |  |
| `runsIn` | RunsIn | no |  |
| `scopeID` | string | no | Scope tagging: set by inline expansion when a uses-level runsIn.image
makes the whole template one isolated scope. Steps sharing ScopeID run
in one environment. Not user-authored. |
| `scopeImage` | string | no |  |
| `shell` | []string | no | Shell overrides the effective interpreter argv for this step. See
Step.Shell for the full resolution priority. |
| `timeoutMinutes` | number | no |  |
| `uploadArtifact` | UploadArtifactStep | no |  |
| `uses` | UsesStep | no |  |

### ApprovalStep

ApprovalStep pauses the run until an authenticated user approves or rejects.
TimeoutMinutes defaults to 60 (applied at compile time) when zero.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `message` | string | no |  |
| `timeoutMinutes` | number | no |  |

### CacheStep

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `key` | string | yes | cache key; supports template expansion |
| `path` | string | yes | directory to cache; supports template expansion |
| `restoreKeys` | []string | no | fallback key prefixes; support template expansion |
| `ttlDays` | integer | no | default 30, max 365 |

### CallStep

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `job` | string | yes |  |
| `with` | map[string]any | no |  |

### DownloadArtifactStep

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `destDir` | string | no | defaults to the current directory if omitted |
| `name` | string | yes |  |

### ForeachDef

ForeachDef expands a step into one parallel run per item in the list.
Key is the variable name accessible in templates as {{ .Foreach.key }}.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `in` | ForeachSource | yes |  |
| `key` | string | yes |  |

### ForeachSource

ForeachSource is either a literal list (YAML sequence) or a template expression (YAML string).

  in: [prod, staging, dev]                    → Literal
  in: $envs                                   → Expr (JSON-array param reference)
  in: "{{ .Params.envs | split \",\" }}"      → Expr (template)
  in: "{{ .Steps.list.Outputs.envs | split \",\" }}" → Expr (step output reference)

### MatrixDef

MatrixDef expands a step into one copy per combination of dimension values
(cartesian product minus exclude entries). Dimensions preserve YAML
declaration order; the combination key joins values with "/" in that order
(e.g. "linux/amd64").

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `exclude` | []map[string]string | no |  |

### Step

Step is a concrete step. Used inside parallel: blocks and as the body of a StepEntry.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `approval` | ApprovalStep | no |  |
| `cache` | CacheStep | no |  |
| `call` | CallStep | no |  |
| `container` | string | no |  |
| `continueOnError` | boolean | no |  |
| `downloadArtifact` | DownloadArtifactStep | no |  |
| `env` | map[string]string | no |  |
| `foreach` | ForeachDef | no |  |
| `if` | string | no |  |
| `matrix` | MatrixDef | no |  |
| `name` | string | yes |  |
| `outputs` | map[string]string | no |  |
| `post` | PostStep | no |  |
| `retry` | RetrySpec | no |  |
| `run` | string | no |  |
| `runsIn` | RunsIn | no |  |
| `scopeID` | string | no | Scope tagging: set by inline expansion when a uses-level runsIn.image
makes the whole template one isolated scope. Steps sharing ScopeID run
in one environment. Not user-authored. |
| `scopeImage` | string | no |  |
| `shell` | []string | no | Shell overrides the effective interpreter argv for this step. Array
form only (v1): e.g. [bash, -lc] or [python3, -c]; the run: script is
appended as the final argv element. Resolution priority (most specific
wins): step.shell > a uses: template's own declared shell > spec.shell
(job-level) > system default. Steps inside parallel: and finally:
count as steps for this purpose. |
| `timeoutMinutes` | number | no |  |
| `uploadArtifact` | UploadArtifactStep | no |  |
| `uses` | UsesStep | no |  |

### PostStep

PostStep defines cleanup/post-processing to run after a step completes.
Executed in LIFO order after RunDAG completes.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `env` | map[string]string | no |  |
| `run` | string | no |  |
| `shell` | []string | no | Shell overrides the interpreter argv for this post hook. When absent,
the hook inherits its owning step's effective shell. The override
exists because inheritance alone breaks down for non-shell
interpreters: a step running under shell: [python3, -c] with a
shell-script cleanup hook needs post: {shell: [sh, -c], run: ...} to
be expressible at all. |

### RetrySpec

RetrySpec configures automatic re-runs of a failing run: step.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `attempts` | integer | yes | Attempts is the total number of tries (1 = no retry). Must be >= 1. |
| `backoff` | string | no | Backoff is a fixed wait between tries as a Go duration (e.g. "30s").
Empty means 0 (immediate retry). |

### RunsIn

RunsIn declares the execution context for a uses: template entry. It is no
longer legal on a plain step (step-level runsIn: was removed; the flat
container: field is the canonical way to pin a plain step to a podTemplate
container). On a uses: entry, only the image form is accepted: it declares
that the whole inlined template runs in one fresh isolated scope built from
this image (host: `<rt> run`; k8s: a throwaway pod). No workspace is shared
— pass inputs via with:/env, return outputs via outputs:/stdout.
runsIn.container on a uses: entry is rejected; set container: on the
template's own steps instead.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `container` | string | no |  |
| `image` | string | no |  |
| `resources` | ResourceSpec | no |  |

### ResourceSpec

ResourceSpec declares CPU/memory requests and limits for a runsIn.image step.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `limits` | ResourceList | no |  |
| `requests` | ResourceList | no |  |

### ResourceList

ResourceList is a cpu/memory pair using Kubernetes quantity strings
(e.g. "500m", "1", "256Mi", "1Gi").

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `cpu` | string | no |  |
| `memory` | string | no |  |

### UploadArtifactStep

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes |  |
| `path` | string | yes |  |

### UsesStep

UsesStep inlines a git-template job's steps directly into the current run.
Job must be a git:// URI; unlike CallStep, it never references a registered job name.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `job` | string | yes |  |
| `with` | map[string]any | no |  |

### Params

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `inputs` | []Input | no |  |
| `outputs` | []Output | no |  |

### Input

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `default` | any | no |  |
| `description` | string | no |  |
| `name` | string | yes |  |
| `pattern` | string | no | Pattern is a regular expression every supplied value must match (defaults
are checked too, so a bad default cannot slip through). Param values are
interpolated into step shell text, so a param fed from an untrusted
source — a webhook payload especially — is a command-injection vector
unless constrained. Suggested starting point: ^[A-Za-z0-9._/-]+$ |
| `required` | boolean | no |  |
| `type` | `string` \| `bool` \| `int` \| `array` | yes |  |
| `unvalidated` | boolean | no | Unvalidated explicitly opts this input out of the pattern requirement for
payload-mapped params. Use only when the value is genuinely free-form and
never reaches a shell. |

### Output

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes |  |
| `type` | `string` \| `bool` \| `int` \| `artifact` | yes | "string", "bool", "int", "artifact" |

### JobTemplatePodTemplate

JobTemplatePodTemplate is the pod-shape subset a template may contribute to
the caller's pod: containers and the volumes they mount. Nothing else.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `spec` | JobTemplatePodSpec | no |  |

### JobTemplatePodSpec

JobTemplatePodSpec holds the mergeable pod-shape lists.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `containers` | []map[string]any | no |  |
| `volumes` | []map[string]any | no |  |

## Schedule

Schedule is the DSL type for a cron schedule trigger.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `apiVersion` | string | yes |  |
| `kind` | string | yes |  |
| `metadata` | Metadata | yes |  |
| `spec` | ScheduleSpec | yes |  |

### Metadata

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `annotations` | map[string]string | no |  |
| `labels` | map[string]string | no |  |
| `name` | string | yes |  |

### ScheduleSpec

ScheduleSpec is the spec section of Schedule.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `cron` | string | yes |  |
| `job` | string | yes |  |
| `params` | map[string]string | no |  |

## WebhookReceiver

WebhookReceiver is the DSL type for webhook receiver configuration.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `apiVersion` | string | yes |  |
| `kind` | string | yes |  |
| `metadata` | Metadata | yes |  |
| `spec` | WebhookReceiverSpec | yes |  |

### Metadata

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `annotations` | map[string]string | no |  |
| `labels` | map[string]string | no |  |
| `name` | string | yes |  |

### WebhookReceiverSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `auth` | WebhookAuth | yes |  |
| `filters` | []string | no |  |
| `paramsMapping` | map[string]string | no |  |
| `trigger` | WebhookTrigger | yes |  |

### WebhookAuth

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `allowUnauthenticated` | boolean | no | required alongside type: none — makes an unauthenticated webhook a deliberate, greppable opt-in |
| `header` | string | no | token type only: header to compare (default X-Gitlab-Token) |
| `secretRef` | string | no |  |
| `type` | `none` \| `hmac-sha256` \| `github` \| `token` | yes | none | hmac-sha256 | github | token |

### WebhookTrigger

WebhookTrigger selects what a webhook delivery triggers. Exactly one of Job or
AppSource must be set: Job creates a Run; AppSource forces a GitOps re-sync of
the named AppSource on the next reconciler tick.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `appSource` | string | no |  |
| `job` | string | no |  |

## AppSource

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `apiVersion` | string | yes |  |
| `kind` | string | yes |  |
| `metadata` | Metadata | yes |  |
| `spec` | AppSourceSpec | yes |  |

### Metadata

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `annotations` | map[string]string | no |  |
| `labels` | map[string]string | no |  |
| `name` | string | yes |  |

### AppSourceSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `path` | string | yes |  |
| `repoURL` | string | yes |  |
| `syncPolicy` | AppSyncPolicy | no |  |
| `targetRevision` | string | yes |  |

### AppSyncPolicy

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `allowManualOverride` | boolean | no | AllowManualOverride disables the managed-resource write guard for
resources managed by this AppSource (direct apply/delete is allowed). |
| `interval` | string | no |  |
| `prune` | boolean | no |  |

## GitCredential

GitCredential is the DSL type that defines git credentials for private repositories.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `apiVersion` | string | yes |  |
| `kind` | string | yes |  |
| `metadata` | Metadata | yes |  |
| `spec` | GitCredentialSpec | yes |  |

### Metadata

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `annotations` | map[string]string | no |  |
| `labels` | map[string]string | no |  |
| `name` | string | yes |  |

### GitCredentialSpec

GitCredentialSpec is the spec section of GitCredential.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `host` | string | yes | hostname to use these credentials for (e.g. github.com) |
| `secretRef` | string | yes | name of the StoredSecret that holds the value |
| `type` | `token` \| `sshKey` | yes | authentication type |

