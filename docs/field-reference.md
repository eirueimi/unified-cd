# unified-cd Field Reference

> This file is auto-generated. Do not edit directly.
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
| `agentSelector` | []string | no | Required agent labels. Each element supports `{{ .Params.X }}` expansion using Run input parameters. |
| `concurrency` | Concurrency | no |  |
| `failFast` | boolean | no | nil = true (default) |
| `params` | Params | yes |  |
| `podTemplate` | PodTemplate | no |  |
| `steps` | []Step | yes |  |
| `timeoutMinutes` | number | no | Job-level timeout in minutes |

### Concurrency

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `mutex` | string | no |  |
| `namedLocks` | []NamedLock | no |  |
| `orLocks` | []OrLock | no |  |

### NamedLock

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `capacity` | integer | yes |  |
| `pool` | string | yes |  |

### OrLock

OrLock acquires exactly one of Candidates — whichever is free — instead of
requiring all of them like NamedLocks does. The acquired candidate value is
exposed to the Job's steps as a synthesized parameter named
strings.ToUpper(Name)+"_LOCK_VALUE" (e.g. Name "env" -> "ENV_LOCK_VALUE"),
readable via {{ .Params.ENV_LOCK_VALUE }}.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `candidates` | []string | yes |  |
| `name` | string | yes |  |

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
| `type` | `string` \| `bool` \| `int` | yes |  |

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

### Step

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `cache` | CacheStep | no |  |
| `call` | CallStep | no |  |
| `container` | string | no |  |
| `continueOnError` | boolean | no |  |
| `downloadArtifact` | DownloadArtifactStep | no |  |
| `env` | map[string]string | no | Environment variables for the step (may include secret references) |
| `if` | string | no |  |
| `name` | string | yes |  |
| `needs` | []string | no |  |
| `outputs` | map[string]string | no | key → template expression |
| `post` | PostStep | no |  |
| `run` | string | no |  |
| `timeoutMinutes` | number | no | Step-level timeout in minutes |
| `uploadArtifact` | UploadArtifactStep | no |  |
| `uses` | UsesStep | no |  |

### CacheStep

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `key` | string | yes |  |
| `path` | string | yes |  |
| `restoreKeys` | []string | no |  |
| `ttlDays` | integer | no | default 30 |

### CallStep

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `job` | string | yes |  |
| `with` | map[string]any | no |  |

### DownloadArtifactStep

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `destDir` | string | no | Defaults to current directory |
| `name` | string | yes |  |

### PostStep

PostStep defines cleanup/post-processing to run after a step completes.
Executed in LIFO order after RunDAG completes.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `env` | map[string]string | no |  |
| `run` | string | no |  |

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

## Schedule

Schedule is the DSL type for cron schedule triggers.

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

ScheduleSpec is the spec section of a Schedule.

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
| `secretRef` | string | no |  |
| `type` | `none` \| `hmac-sha256` \| `github` | yes | none \| hmac-sha256 \| github (X-Hub-Signature-256) |

### WebhookTrigger

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `job` | string | yes |  |

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
| `gitCredentialRef` | string | no |  |
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

GitCredential is the DSL type for defining git authentication credentials for private repositories.

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

GitCredentialSpec is the spec section of a GitCredential.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `host` | string | yes | Hostname to use these credentials for (e.g. github.com) |
| `secretRef` | string | yes | Name of the StoredSecret holding the credential value |
| `type` | `token` \| `sshKey` | yes | Authentication type |
