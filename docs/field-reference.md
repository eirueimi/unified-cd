# unified-cd Field Reference

> This file is auto-generated. Do not edit it directly.
> Regenerate with `go generate ./internal/dsl/`.

## Table of Contents

- [Job](#job)
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
| `labels` | map[string]string | no |  |
| `name` | string | yes |  |

### Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `agentSelector` | []string | no |  |
| `concurrency` | Concurrency | no |  |
| `finally` | []StepEntry | no | Finally runs after the main DAG completes, on success, failure, or
cancellation. Same structure as Steps. A finally step's `if:` defaults to
always-run; use if: failure()/success() to filter. A finally step that
fails marks the run Failed (after all finally steps run). |
| `params` | Params | yes |  |
| `podTemplate` | PodTemplate | no |  |
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
| `run` | string | no |  |
| `runsIn` | RunsIn | no |  |
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
| `ttlDays` | integer | no | default 30 |

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
| `run` | string | no |  |
| `runsIn` | RunsIn | no |  |
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

### RunsIn

RunsIn declares the execution context for a step. Image and Container are
mutually exclusive; both empty (or RunsIn nil) means the default/shared
environment (host process, or the default pod container on k8s).

	image:     run in a fresh isolated env from this image (host: `<rt> run`;
	           k8s: a throwaway pod). No workspace is shared — pass inputs via
	           with:/env, return outputs via outputs:/stdout.
	container: exec into a pre-provisioned named env (k8s pod container only;
	           an error on the host agent).

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
| `required` | boolean | no |  |
| `type` | `string` \| `bool` \| `int` \| `array` | yes |  |

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
| `labels` | map[string]string | no |  |
| `name` | string | yes |  |

### GitCredentialSpec

GitCredentialSpec is the spec section of GitCredential.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `host` | string | yes | hostname to use these credentials for (e.g. github.com) |
| `secretRef` | string | yes | name of the StoredSecret that holds the value |
| `type` | `token` \| `sshKey` | yes | authentication type |

