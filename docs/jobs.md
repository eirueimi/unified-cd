# Job Reference

A comprehensive reference for the `Job` resource — the primary unit of work in unified-cd.

## Table of Contents

- [Job Structure](#job-structure)
- [Metadata](#metadata)
- [Parameters (inputs / outputs)](#parameters-inputs--outputs)
- [Steps](#steps)
  - [Shell Execution (`run`)](#shell-execution-run)
  - [Step Dependencies (`needs`)](#step-dependencies-needs)
  - [Conditional Execution (`if`)](#conditional-execution-if)
  - [Environment Variables (`env`)](#environment-variables-env)
  - [Step Outputs](#step-outputs)
  - [Timeout](#timeout)
  - [Continue on Error](#continue-on-error)
  - [Post-step hooks (`post`)](#post-step-hooks-post)
- [Calling Other Jobs (`call`)](#calling-other-jobs-call)
- [Git Template Inlining (`uses`)](#git-template-inlining-uses)
- [Artifacts](#artifacts)
- [Cache](#cache)
- [Concurrency Control](#concurrency-control)
  - [Mutex](#mutex)
  - [Named Lock Pool](#named-lock-pool)
  - [OR Lock](#or-lock)
- [Agent Selection (`agentSelector`)](#agent-selection-agentselector)
- [Kubernetes Pod Template (`podTemplate`)](#kubernetes-pod-template-podtemplate)
- [Approval Step (`approval`)](#approval-step-approval)
- [Finally Block (`finally`)](#finally-block-finally)
- [Status Functions in `if:`](#status-functions-in-if)
- [Job-level Timeout](#job-level-timeout)
- [Template Syntax](#template-syntax)
- [Secrets in Jobs](#secrets-in-jobs)

---

## Job Structure

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: <string>                  # unique job name (required)
  labels:                         # optional key-value labels
    key: value
spec:
  params: { ... }                 # input/output parameter declarations
  agentSelector: [ ... ]          # required agent label filters
  concurrency: { ... }            # concurrency control
  timeoutMinutes: 60              # job-level timeout in minutes
  podTemplate: { ... }            # Kubernetes pod config (k8s-agent only)
  steps:
    - name: <string>              # step name (required, unique within job)
      if: <expression>            # run condition
      needs: [step1, step2]       # prerequisite steps
      env: { KEY: VALUE }         # environment variables
      run: <shell script>         # shell command
      outputs: { key: expr }      # capture output values
      call: { ... }               # call another registered job
      uses: { ... }               # inline a git template
      cache: { ... }              # cache a directory
      uploadArtifact: { ... }     # upload a file as an artifact
      downloadArtifact: { ... }   # download a previously uploaded artifact
      post: { ... }               # post-run cleanup hook
      container: <string>         # target container (k8s multi-container)
      continueOnError: false      # don't fail the run if this step fails
      timeoutMinutes: 10          # step-level timeout in minutes
```

---

## Metadata

| Field | Type | Required | Description |
|---|---|---|---|
| `metadata.name` | string | Yes | Unique job identifier. Used in CLI, API, and when calling this job from another job. |
| `metadata.labels` | map[string]string | No | Arbitrary labels. Not used for routing; reserved for future filtering. |

---

## Parameters (inputs / outputs)

Declare typed inputs that callers must or may supply, and outputs that the job produces.

```yaml
spec:
  params:
    inputs:
      - name: image
        type: string        # "string" | "bool" | "int"
        required: true
        description: "Docker image name"
      - name: tag
        type: string
        default: latest
      - name: run_tests
        type: bool
        default: true
    outputs:
      - name: image_ref
        type: string        # "string" | "bool" | "int" | "artifact"
      - name: test_report
        type: artifact
```

### Input fields

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | Yes | Parameter name. Referenced as `{{ .Params.name }}` in steps. |
| `type` | string | Yes | `string`, `bool`, or `int` |
| `required` | bool | No | If true, the run fails immediately when the value is not supplied. |
| `default` | any | No | Value used when the caller does not supply this parameter. |
| `description` | string | No | Human-readable description shown in the Web UI trigger form. |

### Output fields

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | Yes | Output name. Accessible in calling jobs as `{{ .Steps.stepName.Outputs.name }}`. |
| `type` | string | Yes | `string`, `bool`, `int`, or `artifact` |

### Trigger with parameters

```bash
unified-cd run trigger build --param image=myapp --param tag=v2.0 --param run_tests=false
```

---

## Steps

Steps are the individual execution units within a job. They run in dependency order (DAG).
Steps with no `needs` dependencies run immediately and in parallel where agent concurrency permits.

### Shell Execution (`run`)

```yaml
steps:
  - name: build
    run: |
      go build -o bin/app ./cmd/server
      echo "Build complete"
```

- Runs in a temporary workspace directory on the agent.
- Uses `/bin/sh` on Linux/macOS, Git Bash on Windows.
- Exit code non-zero fails the step.
- Environment variable `UNIFIED_AGENT_OS` (`linux` / `darwin` / `windows`) is always injected.

### Step Dependencies (`needs`)

```yaml
steps:
  - name: lint
    run: golangci-lint run

  - name: test
    run: go test ./...

  - name: build
    needs: [lint, test]     # waits for both lint and test to succeed
    run: go build ./...

  - name: publish
    needs: [build]
    run: docker push myapp
```

Steps without `needs` start immediately and may run in parallel.
Steps only start when all their `needs` have succeeded (or if `continueOnError: true` is set on a failed predecessor).

### Conditional Execution (`if`)

Steps can be conditionally skipped based on a boolean expression.

> **`if:` is CEL, not a Go template.** Unlike `run:`, `env:`, and `outputs:`
> (which use `{{ .Params.X }}`-style Go templates), `if:` expressions are
> [CEL](https://github.com/google/cel-go) — no `{{ }}` delimiters, and
> variables are lowercase (`params`, `steps`, `secrets`), not `.Params`/`.Steps`.
> **If a Go-template-style `if:` (or any expression that fails to compile) is
> used by mistake, the condition fails OPEN: the step still runs, and the
> only trace is a warning in the agent log.** A production-only step could
> silently run on every trigger. Always use valid CEL syntax below, and check
> agent logs for `if: expression ... compile error` after adding a new
> condition.

```yaml
steps:
  - name: deploy
    needs: [build]
    if: 'params.env == "production"'
    run: ./deploy.sh

  - name: smoke-test
    needs: [deploy]
    if: 'steps.deploy.outputs.result == "ok"'
    run: ./smoke-test.sh
```

**Available variables in `if` expressions (CEL):**

| Variable | Type | Description |
|---|---|---|
| `params.NAME` | string | Input parameter value |
| `steps.STEPNAME.outputs.KEY` | dyn | Output from a previous step (only declared `outputs:`; there is no built-in step-status field) |
| `secrets.NAME` | string | Resolved secret value |

The expression must evaluate to a boolean. Use CEL operators and the
zero-arg status functions (see [Status Functions in `if:`](#status-functions-in-if)):
- `params.env == "production"` — equality check
- `params.env != "production"` — inequality
- `params.env == "production" && params.region == "us-east-1"` — logical AND
- `failure()` / `success()` / `always()` — run based on the job's status so far

### Environment Variables (`env`)

```yaml
steps:
  - name: deploy
    env:
      APP_ENV: "{{ .Params.env }}"
      IMAGE: "{{ .Steps.build.Outputs.image_ref }}"
      DB_URL: "{{ secrets.DATABASE_URL }}"   # secret reference
    run: ./deploy.sh
```

Environment variables are merged with the agent's inherited environment.
Secret references are fetched and injected at runtime, then masked in logs.

### Step Outputs

Capture values from a step and pass them to downstream steps.

```yaml
steps:
  - name: get-version
    run: git describe --tags --abbrev=0
    outputs:
      version: "{{ .Stdout | trim }}"   # capture stdout

  - name: build
    needs: [get-version]
    run: |
      docker build -t myapp:{{ .Steps.get-version.Outputs.version }} .
```

**Output expression variables:**

| Variable | Description |
|---|---|
| `.Stdout` | Full stdout of the step |
| `.Params.NAME` | Input parameter value |

Access previous step outputs in downstream steps:
- `{{ .Steps.STEP_NAME.Outputs.KEY }}`

### Timeout

```yaml
spec:
  timeoutMinutes: 60        # job-level: fails entire job if exceeded

  steps:
    - name: long-test
      timeoutMinutes: 30    # step-level: fails only this step
      run: go test -timeout 25m ./...
```

### Continue on Error

```yaml
steps:
  - name: optional-lint
    continueOnError: true   # run will continue even if this step fails
    run: golangci-lint run
```

### Post-step hooks (`post`)

Define cleanup that runs after the main DAG completes, in LIFO order.

```yaml
steps:
  - name: checkout
    run: git clone https://github.com/org/repo /workspace/repo
    post:
      run: rm -rf /workspace/repo   # cleanup after entire run finishes

  - name: start-db
    needs: [checkout]
    run: docker run -d --name test-db postgres:16
    post:
      run: docker rm -f test-db

  - name: test
    needs: [start-db]
    run: go test ./...
```

Post hooks run after the main DAG finishes (regardless of success or failure), in reverse declaration order.
Use them for cleanup tasks (delete temp files, stop containers, release resources).

---

## Calling Other Jobs (`call`)

Invoke another registered job as a step. The called job runs with its own DAG on the same or a different agent.

```yaml
steps:
  - name: build-frontend
    call:
      job: build                # name of another registered Job
      with:
        image: myapp-frontend
        tag: "{{ .Params.tag }}"

  - name: build-backend
    call:
      job: build
      with:
        image: myapp-backend
        tag: "{{ .Params.tag }}"

  - name: deploy
    needs: [build-frontend, build-backend]
    run: ./deploy.sh
```

`call` steps wait for the called job to complete. The called job's run shares the parent run's context.

> **⚠️ Slot deadlock: the called job needs a *free* agent slot.**
> A `call` step holds the parent run's agent slot while it waits for the called
> job to finish. The called job is a **separate run** that must be claimed by an
> agent — if it can only run on the same agent pool and that pool has no free
> slot, it deadlocks: the child stays `Queued` forever while the parent stays
> `Running`, with no timeout or warning.
>
> The common trigger is an agent (or pool) with **`max-concurrent: 1`** calling a
> job that targets the same agent: the parent occupies the only slot, so the
> child can never be claimed.
>
> **Requirement:** any agent pool that runs `call` chains must have
> **`max-concurrent` ≥ 2** (and ≥ 1 + the maximum `call` nesting depth for
> nested calls), or route the called job to a *different* agent pool via its
> `agentSelector`. Cancelling the parent releases its slot, after which the child
> completes (and the parent's `finally` block still runs).

---

## Git Template Inlining (`uses`)

Inline steps from a job definition stored in a Git repository directly into the current run.

```yaml
steps:
  - name: lint
    uses:
      job: git://github.com/my-org/ci-templates/jobs/golangci-lint.yaml@v2.1.0
      with:
        target: ./...

  - name: security-scan
    needs: [lint]
    uses:
      job: git://github.com/my-org/ci-templates/jobs/trivy.yaml@a1b2c3d4
      with:
        severity: HIGH,CRITICAL
```

**URI format:** `git://<host>/<owner>/<repo>/<path>@<ref>`

- `@v1.2.3` — recommended: pinned tag
- `@a1b2c3d4e5f6...` — pinned commit SHA
- `@main` — mutable branch (not cached; use with caution)

The referenced YAML file must be a valid Job definition. Its steps are inlined at the point of `uses`.

For private repositories, create a [GitCredential](#gitcredential-resource) resource for the host.

---

## Artifacts

Upload and download files between jobs within the same or across runs.

```yaml
steps:
  - name: build
    run: go build -o bin/app ./cmd/server

  - name: upload-binary
    needs: [build]
    uploadArtifact:
      name: app-binary          # artifact name
      path: bin/app             # local path to upload

---
# In a separate job or a later step:
  - name: download-binary
    downloadArtifact:
      name: app-binary          # must match the upload name
      destDir: /tmp/artifacts   # where to place the file (default: current directory)

  - name: run-binary
    needs: [download-binary]
    run: /tmp/artifacts/app --version
```

Artifacts are stored in the S3-compatible object store. Artifact names must be unique within a run.

Artifacts work on both the standard and Kubernetes agents; on the k8s-agent, transfers are handled by an auto-injected workspace sidecar (`unified-artifact`).

### Listing and downloading artifacts (humans)

Besides the job-to-job `uploadArtifact` / `downloadArtifact` steps above, a human operator can
list and fetch a run's artifacts directly through the API or the CLI.

**API:**

```
GET /api/v1/runs/{runID}/artifacts
GET /api/v1/runs/{runID}/artifacts/{name}
```

- `GET /artifacts` lists the artifact names for the run as JSON: `[{"name": "app-binary"}, {"name": "test-report"}]` (an empty run returns `[]`).
- `GET /artifacts/{name}` streams the artifact as a tar+zstd archive (the same format `uploadArtifact`/`downloadArtifact` steps use).
- Both routes accept **either** an agent bearer token **or** a human identity (PAT, OIDC `id_token`, or session cookie) — whichever `ServerAuth` would otherwise accept.
- `PUT /api/v1/runs/{runID}/artifacts/{name}` (upload) is unchanged and remains **agent-only**, authenticated with `BearerAuth` using the agent token. It is not reachable with a PAT, OIDC token, or session — only agents upload artifacts.

**CLI:**

```bash
unified-cd artifact list <run-id>
unified-cd artifact download <run-id> <name> [--dest .]
```

```bash
# List artifacts produced by a run
unified-cd artifact list a1b2c3d4
# app-binary
# test-report

# Download and extract "app-binary" into ./out
unified-cd artifact download a1b2c3d4 app-binary --dest ./out
# extracted app-binary of run a1b2c3d4 to ./out
```

`--dest` defaults to the current directory. Both commands authenticate using the CLI's configured server token (PAT or OIDC login), the same as other `unified-cd` commands.

---

## Cache

Cache directories (e.g. dependency downloads) across runs on the same agent or across agents when S3 is configured.

```yaml
steps:
  - name: restore-cache
    cache:
      path: vendor/             # directory to cache
      key: go-vendor-{{ checksum "go.sum" }}
      restoreKeys:              # fallback keys (prefix match)
        - go-vendor-
      ttlDays: 30               # cache expiry (default: 30 days)

  - name: download-deps
    needs: [restore-cache]
    run: |
      if [ ! -d vendor ]; then
        go mod vendor
      fi

  - name: build
    needs: [download-deps]
    run: go build ./...
```

The `key` and `restoreKeys` strings support template expressions (e.g. `{{ checksum "go.sum" }}`).
On hit, the cached directory is restored before the step runs. On miss, the directory is saved after the run completes.

Cache is now supported on the k8s agent (previously a silent no-op) with the same `key`/`restoreKeys`/`ttlDays` semantics — see [Kubernetes Integration: Artifacts and Cache](kubernetes-integration.md#artifacts-and-cache) for how transfers work and the required S3 credentials. Restore is best-effort (a miss or error never fails the step); save is deferred until the run's main stages complete.

---

## Concurrency Control

Prevent multiple runs from executing simultaneously when they share a resource.

### Mutex

A named mutual exclusion lock — only one run holding the mutex runs at a time.

```yaml
spec:
  concurrency:
    mutex: deploy-production
```

Runs that cannot acquire the mutex wait in the queue until it is released.

### Named Lock Pool

A semaphore — allows up to `capacity` runs to proceed simultaneously.

```yaml
spec:
  concurrency:
    namedLocks:
      - pool: test-environments    # pool name
        capacity: 3                # max concurrent holders
```

Useful for limiting usage of a shared resource (e.g. 3 test environments available).

### OR Lock

Acquire exactly one of several named resources. The acquired value is injected as a parameter.

```yaml
spec:
  concurrency:
    orLocks:
      - name: env                          # parameter name prefix
        candidates:
          - staging-a
          - staging-b
          - staging-c
  steps:
    - name: deploy
      run: |
        echo "Deploying to {{ .Params.ENV_LOCK_VALUE }}"
        ./deploy.sh --target {{ .Params.ENV_LOCK_VALUE }}
```

The acquired candidate is available as `{{ .Params.<NAME>_LOCK_VALUE }}` (uppercased name + `_LOCK_VALUE`).

---

## Agent Selection (`agentSelector`)

A list of labels that a qualifying agent must have. All labels must match (AND logic).

```yaml
spec:
  agentSelector:
    - kind:linux
    - env:prod
```

Labels can include parameter expansion:

```yaml
spec:
  params:
    inputs:
      - name: pool
        type: string
        required: true
  agentSelector:
    - "pool:{{ .Params.pool }}"
```

```bash
unified-cd run trigger build --param pool=gpu-workers
# → only agents with label "pool:gpu-workers" can claim this run
```

If `agentSelector` is omitted, any available agent can claim the run.

---

## Kubernetes Pod Template (`podTemplate`)

For jobs running on the `k8s-agent`. Defines the Kubernetes Pod that executes the steps.

See the [Kubernetes Integration Guide](kubernetes-integration.md) for full details.

```yaml
spec:
  agentSelector:
    - kind:k8s
  podTemplate:
    name: golang              # reference a named template from k8s-agent config

    # Or define inline:
    workspace:
      mountPath: /workspace
      pvc:
        storageClassName: standard
        storageRequest: 10Gi
        accessMode: ReadWriteOnce
    spec:
      containers:
        - name: job
          image: golang:1.24-alpine

    reuse: false              # keep the pod alive after run; reuse for next run
    cleanWorkspace: false     # wipe /workspace before each run
    override:                 # merge additional containers/volumes into base spec
      containers:
        - name: trivy
          image: aquasec/trivy:latest
```

### podTemplate fields

| Field | Type | Description |
|---|---|---|
| `name` | string | Name of a template defined in the k8s-agent config file |
| `spec` | map | Inline Kubernetes PodSpec (used when `name` is empty) |
| `workspace.mountPath` | string | Path inside the pod where workspace is mounted |
| `workspace.pvc.claimName` | string | Existing PVC to mount |
| `workspace.pvc.storageClassName` | string | StorageClass for ephemeral PVC creation |
| `workspace.pvc.storageRequest` | string | Storage size (e.g. `10Gi`) |
| `workspace.pvc.accessMode` | string | `ReadWriteOnce`, `ReadOnlyMany`, or `ReadWriteMany` |
| `reuse` | bool | Return pod to a pool after run and reuse it for subsequent runs |
| `cleanWorkspace` | bool | Delete `/workspace` contents before each run (default: false) |
| `override.containers` | []map | Additional containers to merge into the pod spec |
| `override.volumes` | []map | Additional volumes to merge into the pod spec |

Use `container:` in a step to target a specific container:

```yaml
steps:
  - name: build
    run: go build ./...        # runs in first container (default)

  - name: scan
    needs: [build]
    container: trivy           # runs in the "trivy" container
    run: trivy rootfs /workspace/app
```

---

## Approval Step (`approval`)

An approval step pauses the run and waits for a human decision before continuing.
The agent is held (blocked) until the step is approved, rejected, or it times out.

```yaml
spec:
  steps:
    - name: build
      run: ./build.sh

    - name: gate-deploy
      approval:
        message: "Approve deployment to production?"
        timeoutMinutes: 30   # optional; default 60

    - name: deploy
      run: ./deploy.sh
```

### How to approve or reject

Any authenticated user can make a decision through the CLI, the Web UI, or the API:

**CLI:**

```bash
unified-cd approve <run-id> <step-index>
unified-cd reject  <run-id> <step-index> [--comment "reason"]
```

**API:**

```
POST /api/v1/runs/{runID}/approvals/{stepIndex}
```

Body: `{"decision": "approved"}` or `{"decision": "rejected", "comment": "reason"}`

**Web UI:** Approve / Reject buttons appear on the run detail page while the step is waiting.

### Behavior

- An **approval** allows the run to continue with the next step.
- A **rejection** fails the approval step immediately; the run fails and the `finally` block runs.
- A **timeout** also fails the step (the agent fails the step after `timeoutMinutes`); the run fails
  and the `finally` block runs.
- The identity of the decider is recorded (`decidedBy`) in the audit record.

### `approval` fields

| Field | Type | Required | Description |
|---|---|---|---|
| `message` | string | No | Human-readable prompt shown to approvers in the UI and CLI. |
| `timeoutMinutes` | number | No | Minutes to wait before the step is failed automatically. Default: 60. |

### Constraints and v1 limitations

- `approval` is **not allowed** in a `finally` block.
- The agent is held while waiting. Prefer short timeouts or set `timeoutMinutes` explicitly to
  avoid blocking the agent for an extended period.
- When the step times out, the agent fails the step itself, so the run is correctly marked as
  Failed. The approval audit row in `run_approvals` is reconciled separately: a leader-elected
  controller reaper marks any expired `Pending` row as `TimedOut` (with `decidedBy` = `system`)
  within roughly one minute. The reaper only fixes the audit row — it never changes run status.

---

## Finally Block (`finally`)

Steps under `spec.finally` run **after the main `steps` DAG completes** —
whether it succeeded, failed, or was cancelled. Use it for notifications,
cleanup, or rollback.

```yaml
spec:
  steps:
    - name: deploy
      run: ./deploy.sh
  finally:
    - name: notify          # no if: → always runs
      run: ./notify.sh "{{ .Params.env }}"
    - name: rollback
      if: failure()         # only when a step failed
      run: ./rollback.sh
```

- `finally` uses the same structure as `steps` (stages + `parallel`).
- A `finally` step with no `if:` always runs.
- All `finally` steps run to completion; a `finally` step that fails marks the
  run **Failed**.
- On cancellation, `finally` still runs, but `failure()` is `false`.
- `cache:` and `post:` are not supported in `finally` steps (they register
  deferred hooks that run before `finally`; use them in `steps` instead).
- Both the standard and Kubernetes agents detect mid-run cancellation: an
  in-flight step is interrupted, `finally` still runs (with `failure()` false),
  and the run finishes as `Cancelled`.

---

## Status Functions in `if:`

Three zero-argument functions are available in any step `if:` (job-wide scope):

| Function | True when |
|---|---|
| `failure()` | a previous non-`continueOnError` step has failed (not on cancel) |
| `success()` | no step has failed and the run was not cancelled |
| `always()`  | always |

If an `if:` expression does **not** mention a status function, it is implicitly
treated as requiring `success()` — so a normal step is skipped once an earlier
step has failed (GitHub Actions semantics). Add `if: failure()` or
`if: always()` to opt in to running after a failure.

> **Compile/eval errors fail open.** `if:` is CEL — see the warning under
> [Conditional Execution](#conditional-execution-if). An `if:` that doesn't
> compile (e.g. leftover `{{ }}` Go-template syntax) does not fail the run or
> skip the step: the step **runs anyway**, and only a warning is written to
> the agent log. Double-check any non-trivial `if:` expression before relying
> on it to gate a sensitive step (e.g. a production deploy).

---

## Job-level Timeout

```yaml
spec:
  timeoutMinutes: 120
```

If the job has not completed within `timeoutMinutes`, the entire run is cancelled.
Individual steps can also have their own `timeoutMinutes` (step-level timeout is independent).

---

## Template Syntax

Job YAML values support Go template expressions (`{{ expr }}`).

### Available variables

| Variable | Available in | Description |
|---|---|---|
| `{{ .Params.NAME }}` | `run`, `env`, `if`, `agentSelector`, `outputs`, `call.with`, `uses.with`, `cache.key` | Input parameter value |
| `{{ .Steps.NAME.Outputs.KEY }}` | `run`, `env`, `if`, `outputs` | Output from a completed step |
| `{{ .Steps.NAME.Status }}` | `if` | Step status: `Succeeded`, `Failed`, `Skipped` |
| `{{ secrets.NAME }}` | `env` values, `run` strings | Decrypted secret value |

### Template functions

Standard Go template functions are available, plus:

| Function | Example | Description |
|---|---|---|
| `trim` | `{{ .Stdout \| trim }}` | Remove leading/trailing whitespace |
| `trimSpace` | `{{ .Stdout \| trimSpace }}` | Same as `trim` |
| `eq` | `{{ eq .Params.env "prod" }}` | Equality |
| `ne` | `{{ ne .Params.env "prod" }}` | Inequality |
| `and` | `{{ and (eq .Params.a "x") (eq .Params.b "y") }}` | Logical AND |
| `or` | `{{ or (eq .Params.a "x") (eq .Params.b "y") }}` | Logical OR |
| `not` | `{{ not (eq .Params.a "x") }}` | Logical NOT |

---

## Secrets in Jobs

Reference encrypted secrets stored server-side using `{{ secrets.NAME }}` syntax.

```yaml
steps:
  - name: deploy
    env:
      DB_URL: "{{ secrets.DATABASE_URL }}"
      API_KEY: "{{ secrets.API_KEY_PROD }}"
    run: |
      ./deploy.sh
```

**Rules:**
- Secret names must use only alphanumerics and underscores (no hyphens).
- Secrets referenced in `env` values and `run` strings are auto-detected; no explicit declaration needed.
- Secret values are transmitted to the agent over HTTPS at claim time.
- All occurrences of the secret value in log output are automatically masked as `***`.

To create secrets:

```bash
unified-cd secret set DATABASE_URL "postgres://user:pass@host/db"
unified-cd secret set API_KEY_PROD "sk-..."
```

See the [Secrets Management Guide](secrets.md) for the full encryption model.

---

## Complete Example

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: ci-pipeline
spec:
  params:
    inputs:
      - name: image
        type: string
        required: true
      - name: tag
        type: string
        default: latest
      - name: deploy_env
        type: string
        default: staging
    outputs:
      - name: image_ref
        type: string
  agentSelector:
    - kind:linux
    - env:ci
  concurrency:
    mutex: "deploy-{{ .Params.deploy_env }}"
  timeoutMinutes: 60

  steps:
    - name: lint
      run: golangci-lint run ./...
      timeoutMinutes: 10

    - name: test
      run: go test -race ./...
      timeoutMinutes: 20

    - name: build
      needs: [lint, test]
      run: |
        go build -o bin/server ./cmd/server
        echo "Build successful"
      outputs:
        binary_path: bin/server

    - name: upload-binary
      needs: [build]
      uploadArtifact:
        name: server-binary
        path: bin/server

    - name: build-image
      needs: [upload-binary]
      env:
        REGISTRY_PASS: "{{ secrets.REGISTRY_PASS }}"
      run: |
        echo "$REGISTRY_PASS" | docker login registry.example.com --password-stdin -u ci
        docker build -t {{ .Params.image }}:{{ .Params.tag }} .
        docker push {{ .Params.image }}:{{ .Params.tag }}
      outputs:
        image_ref: "{{ .Params.image }}:{{ .Params.tag }}"

    - name: deploy-staging
      needs: [build-image]
      if: 'params.deploy_env == "staging"'
      run: |
        ./deploy.sh --env staging --image {{ .Steps.build-image.Outputs.image_ref }}

    - name: deploy-production
      needs: [build-image]
      if: 'params.deploy_env == "production"'
      run: |
        ./deploy.sh --env production --image {{ .Steps.build-image.Outputs.image_ref }}
```
