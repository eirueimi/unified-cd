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
  - [Matrix and Foreach Steps](#matrix-and-foreach-steps)
- [Calling Other Jobs (`call`)](#calling-other-jobs-call)
- [Git Template Inlining (`uses`)](#git-template-inlining-uses)
- [Isolated Execution (`runsIn`)](#isolated-execution-runsin)
  - [Step-level `runsIn.image`](#step-level-runsinimage)
  - [Uses-level `runsIn.image` (scope)](#uses-level-runsinimage-scope)
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
    - parallel:                   # OR: a group of steps that run concurrently
        - name: <string>          # (see "Concurrent Steps (parallel)")
          run: <shell script>
```

---

## Metadata

| Field | Type | Required | Description |
|---|---|---|---|
| `metadata.name` | string | Yes | Unique job identifier. Used in CLI, API, and when calling this job from another job. |
| `metadata.labels` | map[string]string | No | Arbitrary labels. Not used for routing; reserved for future filtering. |

### Hierarchical grouping (annotations.path)

A job's position in the Web UI tree comes from `metadata.annotations.path`.
Jobs synced by an AppSource get this set automatically from their directory
(relative to the AppSource `spec.path`), so `jobs/team-a/build.yaml` shows as
`build` under a `team-a` folder. The stored, unique job name is the *qualified*
name `team-a/build` — trigger it with `unified-cli run trigger team-a/build`.
Jobs applied directly with no `path` appear at the tree root.

**Upgrade note:** if you're upgrading from a version predating hierarchical
grouping, only jobs at the AppSource root (no subdirectory) keep their old
name unchanged. Jobs that previously synced from a subdirectory (e.g.
`jobs/team-a/build.yaml`, previously stored as `build`) are re-keyed to their
qualified name (`team-a/build`) on the next sync — this is a one-time
prune/re-create of those jobs. Re-point any Schedules or WebhookReceivers
that reference the old flat name before or right after upgrading.

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

Steps are the individual execution units within a job. They run **sequentially, in the
order listed** under `steps:`. To run steps concurrently, group them inside a `parallel:`
block (see [Concurrent Steps (`parallel`)](#concurrent-steps-parallel)).

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

### Concurrent Steps (`parallel`)

> **`needs:` is no longer supported.** Steps run sequentially in declaration
> order by default. To run steps concurrently, group them inside a `parallel:`
> block instead of declaring dependencies between them.

```yaml
steps:
  - parallel:
      - name: lint
        run: golangci-lint run

      - name: test
        run: go test ./...

  - name: build       # starts only after both lint and test have succeeded
    run: go build ./...

  - name: publish
    run: docker push myapp
```

A `parallel:` entry is a top-level item under `steps:` (or `finally:`) that holds a list
of steps under `parallel:` instead of a single `name:`/`run:` step. All steps inside the
block start together and the block completes once every member has finished (or if
`continueOnError: true` is set on a failed member). The next step after the block only
starts once the whole block completes. A `parallel:` entry cannot also declare `name`,
`run`, or the other concrete-step fields — it is exclusively a group of `Step`s.

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
    if: 'params.env == "production"'
    run: ./deploy.sh

  - name: smoke-test
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
    run: docker run -d --name test-db postgres:16
    post:
      run: docker rm -f test-db

  - name: test
    run: go test ./...
```

Post hooks run after the main DAG finishes (regardless of success or failure), in reverse declaration order.
Use them for cleanup tasks (delete temp files, stop containers, release resources).

---

### Matrix and Foreach Steps

`matrix:` expands a single step declaration into one run per combination of
one or more dimensions (a cartesian product), similar to a build matrix in
other CI systems. It works inside `parallel:` blocks too — every step in a
`parallel:` block that declares a `matrix:` (or `foreach:`) expands into its
combinations, and those combinations run in parallel alongside the block's
other steps.

```yaml
steps:
  - name: build
    matrix:
      os: [linux, windows, darwin]
      arch: [amd64, arm64]
      exclude:
        - os: windows
          arch: arm64
    outputs:
      built: "{{ .Matrix.os }}-{{ .Matrix.arch }}"
    run: |
      GOOS={{ .Matrix.os }} GOARCH={{ .Matrix.arch }} go build -o out/{{ .Matrix.os }}-{{ .Matrix.arch }}
```

**Dimensions.** Each key under `matrix:` other than the reserved `exclude` is
a dimension: its name becomes the key used in `{{ .Matrix.<name> }}`, and its
value is a `ForeachSource` — the same source form `foreach.in` accepts:

- a literal list: `os: [linux, windows, darwin]`
- a `$param` reference to a JSON-array-valued parameter: `os: $osList`
- a template expression: `os: "{{ .Params.osList | split \",\" }}"`, including
  references to a previous step's output (`"{{ .Steps.list.Outputs.envs | split \",\" }}"`)

Dimensions are evaluated and combined in **declaration order**, and within
each dimension, in list order. A dimension that resolves to zero items
produces **zero combinations overall** — the step is skipped entirely (not
an error), and the run continues normally.

**`exclude:`** is a list of dimension-name → value maps. A combination is
dropped if it matches every key/value pair in at least one exclude entry.
An exclude entry naming only a subset of dimensions still drops every
combination that matches those dimensions (partial-match exclusion, the
same semantics as GitHub Actions matrix `exclude`). Referencing a dimension
name that isn't declared on the same `matrix:` is an apply-time error.

**Combination key normal form.** Each expanded combination gets a key formed
by joining its dimension values with `/`, in dimension declaration order —
e.g. `linux/amd64`. Because `/` is the separator, **dimension values must
not contain `/`**; a value that does causes the step to fail at expansion
time (this is caught even for dynamic/expression-sourced dimensions, since
values aren't known until expansion).

**Combination limit.** The number of combinations a single `matrix:` step
may expand to (after `exclude` is applied) is capped — default **64** —
configurable on the controller via the `--matrix-max-combinations` flag or
the `UNIFIED_MATRIX_MAX_COMBINATIONS` environment variable. Because
dimensions can be dynamic (parameter- or step-output-sourced), the cap is
enforced at **expansion time** on the agent, not at apply time; exceeding it
fails the step.

**Output aggregation.** A non-matrix step's `outputs:` values are plain
strings, as usual. A **matrix step's outputs are aggregated across all of
its combinations** into a map keyed by combination key:

```yaml
- name: report
  run: |
    echo "built variants: {{ keys .Steps.build.Outputs.built }}"
    echo "one value: {{ index .Steps.build.Outputs.built "linux/amd64" }}"
```

- `{{ .Steps.build.Outputs.built }}` is a `map[string]string` (combination key → value), not a plain string.
- Use the `keys` / `values` template functions to get the sorted list of combination keys, or the values in that same sorted-key order — handy for fanning a downstream `matrix:`/`foreach:` dimension out from a previous matrix step's outputs.
- Use `{{ index .Steps.build.Outputs.built "linux/amd64" }}` to read a single combination's value.
- From a CEL `if:` expression, access it as `steps.build.outputs.built["linux/amd64"]`.
- If a matrix step's output is promoted to a job-level output (declared in `spec.params.outputs` and referenced from a step in that job), the promoted value becomes a **JSON-encoded string** of the combination-key → value map (e.g. `{"linux/amd64":"1.2","linux/arm64":"1.3"}`), not a Go map — job outputs are always plain strings on the wire, so the aggregated map is serialized rather than dropped.

**`foreach:` is sugar for a single-dimension `matrix:`.** `foreach: {key: X, in: [...]}` is equivalent to a one-dimension `matrix:` named `X`, and `{{ .Foreach.X }}` reads the same value as `{{ .Matrix.X }}` would. Declaring both `foreach:` and `matrix:` on the same step is a mutual-exclusion error at apply time.

**`approval` and `matrix`/`foreach` cannot be specified together** — expanded combinations share one (run_id, step_index) approval decision row, which has no way to represent per-combination decisions, so declaring both on the same step is rejected at apply time.

A `call` step with a matrix launches one child run per combination, and the outputs become an aggregated map.

On the Kubernetes agent, combinations run sequentially within the Pod (the standard agent runs them in parallel).

> **Upgrade note:** matrix support changed the agent claim wire format
> (`ForeachKey`/`ForeachValue` were replaced by a `MatrixValues` map). There
> is no backward-compatibility shim — see
> [docs/agents.md](agents.md#matrix-wire-format-upgrade-note) for the
> upgrade requirement.

---

## Calling Other Jobs (`call`)

Invoke another registered job as a step. The called job runs with its own DAG on the same or a different agent.

```yaml
steps:
  - parallel:
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

## Isolated Execution (`runsIn`)

`runsIn` runs a step in a reproducible container instead of directly on the
agent host. It has two forms — `image` (a fresh isolated environment) and
`container` (exec into a named, pre-provisioned k8s pod container). This guide
covers the `image` form; see the [`runsIn` field reference](resources.md#runsin)
for the full field table (including `runsIn.container` and `runsIn.resources`).

An isolated `runsIn.image` step runs in a Linux container regardless of the
agent's host OS, so `UNIFIED_AGENT_OS` reports `linux` inside it.

### Step-level `runsIn.image`

Put `runsIn.image` on a plain `run` step to run just that step in a fresh,
throwaway container (the standard agent runs `<runtime> run --rm`; the k8s agent
uses a throwaway pod):

```yaml
steps:
  - name: lint
    runsIn:
      image: golangci/golangci-lint:latest
    run: golangci-lint run ./...
```

A step-level isolated call is a **pure function**: it does **not** share the job
workspace. Pass inputs via `with:`/`env` and return outputs via `outputs:` or
stdout. It has no persistent filesystem, so `cache`/`uploadArtifact`/
`downloadArtifact` are not supported on a step-level isolated step — use a
uses-level scope (below) when you need those.

### Uses-level `runsIn.image` (scope)

Put `runsIn.image` on a `uses:` step to run the **entire inlined template** in
**one** isolated environment — a "scope" — that stays alive across all of the
template's steps (one container on the standard agent, one dedicated pod on
k8s):

```yaml
steps:
  - name: build
    uses:
      job: git://github.com/my-org/ci-templates/jobs/build.yaml@v1.0.0
      with:
        target: ./cmd/server
    runsIn:
      image: golang:1.22
```

Because the template's steps share one long-lived environment, `cache`,
`uploadArtifact`, and `downloadArtifact` steps inside the template operate on
**the scope's own filesystem**, not the outer job workspace. So if `build.yaml`
restores a dependency cache, compiles, and uploads the resulting binary as an
artifact, all three happen inside the `golang:1.22` scope — a template can build
in its isolated environment and save the result without ever touching the outer
workspace.

The scope starts from a fresh, empty filesystem and never shares the outer job
workspace:

- **Inputs** enter via `with:` (env vars) and `downloadArtifact` (written into
  the scope filesystem).
- **Outputs** leave via `uploadArtifact` (pushed to the run's artifact store)
  and `outputs:`/stdout.

Artifacts are keyed by run, not by workspace path, so they cross the isolation
boundary naturally — on Kubernetes a scoped `uses` needs no shared
`ReadWriteMany` volume. Under `matrix`/`foreach`, each variant of a scoped
`uses` gets its own independent scope (its own container/pod).

A step-level `runsIn.container` (uses-level too) and a `uses` with no `runsIn`
keep their existing behavior — scope mode is triggered only by a **uses-level
`runsIn.image`**.

**Not allowed inside a scoped `uses`** (parse errors, because they are
incompatible with holding one isolated environment across the whole template):

- an inlined step with its own `runsIn.image`/`runsIn.container` — the scope is
  a single homogeneous environment;
- an `approval:` step — it would pin the isolated container/pod open across a
  human wait;
- a `call:` step — the child run executes elsewhere and cannot see the scope's
  filesystem.

---

## Artifacts

Upload and download files between jobs within the same or across runs.

```yaml
steps:
  - name: build
    run: go build -o bin/app ./cmd/server

  - name: upload-binary
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
      path: vendor/             # directory to cache (supports templates, e.g. {{ .Params.working_dir }}/vendor)
      key: go-vendor-{{ hashFile "go.sum" }}
      restoreKeys:              # fallback keys (prefix match)
        - go-vendor-
      ttlDays: 30               # cache expiry (default: 30 days)

  - name: download-deps
    run: |
      if [ ! -d vendor ]; then
        go mod vendor
      fi

  - name: build
    run: go build ./...
```

The `path`, `key`, and `restoreKeys` strings support template expressions (e.g. `path: {{ .Params.working_dir }}/node_modules`, `key: go-vendor-{{ hashFile "go.sum" }}`). A `path` that fails to expand (or expands to empty) fails the step on the standard agent and skips the cache operation on the k8s agent.
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
| `{{ .Params.NAME }}` | `run`, `env`, `if`, `agentSelector`, `outputs`, `call.with`, `uses.with`, `cache.key`, `cache.path`, `cache.restoreKeys` | Input parameter value |
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
- Secret names may contain hyphens (e.g. `slack-webhook-url`) as well as alphanumerics and
  underscores. `{{ secrets.NAME }}` and `{{ .Secrets.NAME }}` both work with hyphenated
  names — hyphenated references are automatically rewritten to an index lookup internally,
  since Go template dot-notation can't address a map key containing a hyphen directly.
- Secrets referenced in `env` values and `run` strings are auto-detected; no explicit declaration needed.
- Secret values are transmitted to the agent over HTTPS at claim time.
- All occurrences of the secret value in log output are automatically masked as `***`.

To create secrets:

```bash
unified-cd secret set DATABASE_URL "postgres://user:pass@host/db"
unified-cd secret set API_KEY_PROD "sk-..."
unified-cd secret set slack-webhook-url "https://hooks.slack.com/services/..."
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
    - parallel:
        - name: lint
          run: golangci-lint run ./...
          timeoutMinutes: 10

        - name: test
          run: go test -race ./...
          timeoutMinutes: 20

    - name: build
      run: |
        go build -o bin/server ./cmd/server
        echo "Build successful"
      outputs:
        binary_path: bin/server

    - name: upload-binary
      uploadArtifact:
        name: server-binary
        path: bin/server

    - name: build-image
      env:
        REGISTRY_PASS: "{{ secrets.REGISTRY_PASS }}"
      run: |
        echo "$REGISTRY_PASS" | docker login registry.example.com --password-stdin -u ci
        docker build -t {{ .Params.image }}:{{ .Params.tag }} .
        docker push {{ .Params.image }}:{{ .Params.tag }}
      outputs:
        image_ref: "{{ .Params.image }}:{{ .Params.tag }}"

    - name: deploy-staging
      if: 'params.deploy_env == "staging"'
      run: |
        ./deploy.sh --env staging --image {{ .Steps.build-image.Outputs.image_ref }}

    - name: deploy-production
      if: 'params.deploy_env == "production"'
      run: |
        ./deploy.sh --env production --image {{ .Steps.build-image.Outputs.image_ref }}
```
