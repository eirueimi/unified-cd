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
- [Fail Fast](#fail-fast)
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
  failFast: true                  # cancel remaining steps on first failure (default: true)
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

```yaml
steps:
  - name: deploy
    needs: [build]
    if: '{{ eq .Params.env "production" }}'
    run: ./deploy.sh

  - name: smoke-test
    needs: [deploy]
    if: '{{ .Steps.deploy.Status | eq "Succeeded" }}'
    run: ./smoke-test.sh
```

**Available variables in `if` expressions:**

| Variable | Type | Description |
|---|---|---|
| `.Params.NAME` | any | Input parameter value |
| `.Steps.STEPNAME.Status` | string | Status of a previous step: `Succeeded`, `Failed`, `Skipped` |
| `.Steps.STEPNAME.Outputs.KEY` | string | Output from a previous step |

The expression must evaluate to a boolean. Use Go template functions:
- `{{ eq .Params.env "production" }}` — equality check
- `{{ ne .Params.env "production" }}` — inequality
- `{{ and (eq .Params.env "production") (eq .Params.region "us-east-1") }}` — logical AND

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

## Fail Fast

When `failFast` is `true` (the default), the first step failure cancels all other in-progress steps.
Set to `false` to allow all steps to run to completion regardless of failures.

```yaml
spec:
  failFast: false    # default is true
  steps:
    - name: lint
      run: golangci-lint run
    - name: test
      run: go test ./...
    # Both lint and test run even if one fails
```

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
  failFast: true
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
      if: '{{ eq .Params.deploy_env "staging" }}'
      run: |
        ./deploy.sh --env staging --image {{ .Steps.build-image.Outputs.image_ref }}

    - name: deploy-production
      needs: [build-image]
      if: '{{ eq .Params.deploy_env "production" }}'
      run: |
        ./deploy.sh --env production --image {{ .Steps.build-image.Outputs.image_ref }}
```
