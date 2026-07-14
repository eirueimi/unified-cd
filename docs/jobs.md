# Job Reference

A comprehensive reference for the `Job` resource — the primary unit of work in unified-cd.

## Table of Contents

- [Job Structure](#job-structure)
- [Metadata](#metadata)
- [Parameters (inputs / outputs)](#parameters-inputs--outputs)
- [Steps](#steps)
  - [Shell Execution (`run`)](#shell-execution-run)
  - [Shell (`shell:`)](#shell-shell)
  - [Step Dependencies (`needs`)](#step-dependencies-needs)
  - [Conditional Execution (`if`)](#conditional-execution-if)
  - [Environment Variables (`env`)](#environment-variables-env)
  - [Step Outputs](#step-outputs)
  - [Timeout](#timeout)
  - [Continue on Error](#continue-on-error)
  - [Retry](#retry)
  - [Post-step hooks (`post`)](#post-step-hooks-post)
  - [Matrix and Foreach Steps](#matrix-and-foreach-steps)
- [Calling Other Jobs (`call`)](#calling-other-jobs-call)
- [Git Template Inlining (`uses`)](#git-template-inlining-uses)
- [Job Isolation: `native` and the claim pod](#job-isolation-native-and-the-claim-pod)
  - [Sidecar container logs](#sidecar-container-logs)
  - [`container:` — targeting a podTemplate container](#container--targeting-a-podtemplate-container)
  - [`native: true` — host-process jobs](#native-true--host-process-jobs)
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
  native: false                   # true = host-process job, no containers at all (see below)
  podTemplate: { ... }            # sidecar containers for an isolated job (both agents honor this)
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
      container: <string>         # exec into a named podTemplate container instead of the primary
      continueOnError: false      # don't fail the run if this step fails
      timeoutMinutes: 10          # step-level timeout in minutes
      retry: { attempts: 3, backoff: 30s }  # retry a run: step on failure (run: only)
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
        type: string        # "string" | "bool" | "int" | "array"
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
| `type` | string | Yes | `string`, `bool`, `int`, or `array` (backs `$param`-style references used by `matrix`/`foreach`/`orLocks`) |
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
unified-cli run trigger build --param image=myapp --param tag=v2.0 --param run_tests=false
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
- **Isolated jobs (the default — see [Job Isolation](#job-isolation-native-and-the-claim-pod)
  below):** the script execs into a container using the step's effective
  interpreter argv. The system default is the injected `ucd-sh` shim
  (`["/.ucd/ucd-sh", "-c"]`) — no shell binary is required in the step's
  image. See [Shell (`shell:`)](#shell-shell) to override it.
- **`native: true` jobs:** the script runs as a host process under host
  `bash -lc` (Git Bash on Windows) by default, unless overridden with
  `shell:`.
- Exit code non-zero fails the step.
- Environment variable `UNIFIED_AGENT_OS` (`linux` / `darwin` / `windows`) is always injected.
- Multi-line `run: |` scripts are executed **without** `set -e`: a failing
  intermediate command does not fail the step as long as the script's last
  command exits 0. Add `set -e` as the first line of your script (or check
  exit codes yourself) if you want an early failure to fail the step.

### Shell (`shell:`)

Override the interpreter argv used to execute a step's (or a whole job's) `run:` script.

```yaml
spec:
  shell: [bash, -lc]                       # job-level default (optional)
  steps:
    - name: build
      shell: [bash, -euo, pipefail, -c]    # step-level override
      run: |
        make build | tee build.log

    - name: quick
      shell: [python3, -c]                 # any interpreter, not just a shell
      run: print("hi")

    - name: default
      run: echo hi                         # -> ["/.ucd/ucd-sh", "-c", "echo hi"]
```

**Shape.** `shell:` is a non-empty array of non-empty strings — the array
form only. There is no scalar/string shorthand and no re-splitting of a
single string; the array is exec'd **verbatim as argv**, with the `run:`
script appended as the final element, never re-parsed or re-quoted.
`shell: [bash, -lc]` execs `bash -lc "<script>"`; `shell: [python3, -c]`
execs `python3 -c "<script>"`. Validation at apply time only checks the
shape (non-empty array of non-empty strings); a program missing from the
target image/host surfaces at **runtime** as a failed step, not an
apply-time error — the container runtime's own error (e.g.
`OCI runtime exec failed: ... exec: "python3": executable file not found
in $PATH`, typically exit code 126 or 127) appears in the step's log. If a
step fails that way, check the `shell:` argv against what the target image
actually contains.

**Resolution priority** (most specific wins):

| Priority | Source | Notes |
|---|---|---|
| 1 (highest) | `step.shell` | Steps inside `parallel:` and `finally:` count as steps for this purpose. |
| 1 | `post.shell` | A `post:` hook may declare its own `shell:`; when absent, it **inherits its owning step's effective shell** (not the job default). This exists because inheritance alone breaks down for non-shell interpreters — a `shell: [python3, -c]` step with a shell-script cleanup hook needs `post: {shell: [sh, -c], run: ...}` to be expressible at all. |
| 2 | A `uses:` template's own declared shell | A template step's own `shell:` survives inlining as-is; a template-level `spec.shell` is stamped onto every inlined step that doesn't already declare one, at expansion time. The caller of the `uses:` step **cannot override either** — the template author chose it because the script needs it. A template that declares neither inherits the caller's job-level default, resolved at claim-build time. |
| 3 | `spec.shell` (job-level) | Applies to every step in the job that doesn't declare its own `shell:` (or wasn't stamped by a `uses:` template). |
| 4 (lowest) | System default | `["/.ucd/ucd-sh", "-c"]` for container execution (any job that isn't `native: true`); host `bash -lc` (Git Bash on Windows) for `native: true` steps — unchanged in v1 (see [Non-goals](#native-true--host-process-jobs)). |

Two special cases fall outside the table above:

- **`call:` does not inherit.** A called job's steps resolve `shell:`
  entirely from the called job's own spec — never from the calling step's
  or job's `shell:`. This is consistent with every other job-level spec
  field: a `call:` child is a separate run.
- **`container:` resolves inside the target container.** A step with
  `container: X` needs its interpreter argv present in `X`'s image, exactly
  like the primary container — the same priority table applies; only the
  exec target differs.

#### The default: the `ucd-sh` shim

The system default for every container-executed step is:

```
["/.ucd/ucd-sh", "-c"]
```

`ucd-sh` is a small, statically-linked Go binary — embedded into every
unified-cd agent binary and injected into every job/scope container at the
reserved path **`/.ucd`** (see below) — that interprets the script using
[`mvdan.cc/sh`](https://github.com/mvdan/sh), a pure-Go POSIX-ish shell
implementation. It requires **no shell binary in the target image**:
bash-less/sh-less images with basic coreutils (`alpine`, busybox-based
images) work as step containers by default. Truly empty images (`scratch`,
distroless-static) can host the keep-alive and remain exec-able, but on the
**Kubernetes agent** they cannot run steps that carry environment variables
— every step does (the agent always injects `UNIFIED_AGENT_OS`), and the
k8s exec path applies env by prepending the `env` binary, which those
images lack (the step fails with exit 127). The host agent applies env via
the container runtime and is unaffected. (See
[Configuration Reference](configuration.md) for the `podImage`/`podTemplate`
implications.)

**Verified interpreter constraints** — supported vs. not, and what to do
about the gaps:

| Category | Supported | Not supported — declare `shell: [bash, -lc]` if needed |
|---|---|---|
| Control flow | `if`/`case`/`for`/`while`/`until`, functions, `local` | — |
| Tests / expansion | `[[ ]]`, arithmetic `$(( ))`, most parameter expansions | — |
| Data | arrays, associative arrays, `set --` argv manipulation, IFS-based word splitting | — |
| Pipes / redirects / substitution | pipes, redirects, heredocs, command substitution, process substitution (Unix) | `/dev/tcp` (Bash's `/dev/tcp/host/port` pseudo-device) |
| `set` options | `set -e`, `-u`, `-x`, `-o pipefail` | — |
| Job control | fan-out/join (`cmd & cmd & wait`); `wait $!` (a virtual job handle — returns the backgrounded command's real exit status) | `wait -n`, `wait -p` (rejected immediately: exit status 2, error message names `wait` and the rejected flag); `jobs`; `kill $!` (no `kill` builtin — `$!` is a virtual `gN` handle no external `kill` understands); `PIPESTATUS` |
| `trap` | `trap ... EXIT`, `trap ... ERR` | Any other condition (signal name or number) — see the sanitizer below |
| `shopt` | a 6-option subset | anything beyond that subset |
| Process model | subshells run as goroutines | no real fork/PID semantics — nothing a script spawns is a real OS process with a kernel PID |

**Pinned background-job behavior:** a script that backgrounds a job and does
**not** `wait` for it (e.g. `long-running-daemon &`) is not awaited when the
script body finishes — `ucd-sh` returns as soon as the main script body
completes, leaving the backgrounded job running as an orphaned in-process
goroutine bounded only by the step's own context (a step timeout or run
cancellation eventually stops it; a step with no timeout that backgrounds an
infinite-looping job **reports success and moves on** while that job keeps
running). Add an explicit `wait` if the step must block until a backgrounded
job finishes.

#### `trap` sanitizer

`mvdan.cc/sh`'s `trap` builtin only implements the `EXIT` and `ERR`
conditions; any other condition (`TERM`, `INT`, a bare signal number, ...)
errors with exit status 2 — which, under `set -e`, would kill the script at
the `trap` line before it does anything. `ucd-sh` sanitizes every `trap`
call before running the script:

- Unsupported condition words (signal names/numbers) are stripped; `EXIT`
  and `ERR` are always kept.
- Each stripped condition emits one `[ucd-sh] `-prefixed warning line to
  stderr, naming the signal and recommending `shell: [bash, -lc]` for steps
  that need real signal traps.
- The bare two-word form (`trap SIGNAL`, no handler — POSIX resets the
  condition to its default disposition) is sanitized the same way: an
  unsupported signal there is stripped rather than left to error.
- If every condition on a `trap` call is stripped, the call becomes a no-op
  (`true`) rather than erroring.

This is graceful degradation, not silent data loss: the warning tells you
exactly what happened and what to change if the step actually needs the
trap.

#### `/.ucd` — reserved path

`/.ucd` is injected into every container a job or scope creates (the
primary container, every `podTemplate`/sidecar container, `uses:`-scope
containers) and holds the `ucd-sh` binary. It is **reserved**: a
`podTemplate` (or claim-pod container) that mounts something else over
`/.ucd` is user error and fails loudly the first time the agent execs into
that container. See [Kubernetes Integration: Step execution
mechanism](kubernetes-integration.md#step-execution-mechanism) and [Agent
Labels and Routing](agents.md) for how `/.ucd` is populated on each
backend.

---

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

> **On the Kubernetes agent, members of a `parallel:` group run sequentially** inside the
> Pod (same as matrix/foreach combinations); the completion semantics (block finishes when
> every member has) are identical, only the wall-clock concurrency differs.

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

### Retry

`retry:` re-runs a `run:` step on failure, up to `attempts` total tries.

```yaml
steps:
  - name: flaky-integration-test
    run: go test ./it/...
    timeoutMinutes: 5     # bounds EACH attempt
    retry:
      attempts: 3
      backoff: 30s
```

| Field | Type | Required | Description |
|---|---|---|---|
| `retry.attempts` | number | Yes | Total number of tries. `1` (the default when `retry:` is omitted) means no retry. |
| `retry.backoff` | duration | No | How long to wait between tries (a Go duration, e.g. `30s`, `2m`). Default: `0` (retry immediately). |

Notes:
- `retry:` is only valid on a `run:` step; declaring it on any other step type is a validation error at apply time.
- Any failure of an attempt is retried: a non-zero exit code, an exec/infra error, or that attempt timing out. A run being cancelled is never retried.
- `timeoutMinutes` bounds **each attempt**, not the overall retry budget — with `attempts: 3` and `timeoutMinutes: 5`, the step can take up to 15 minutes across all tries.
- `continueOnError` is evaluated after the retry budget is exhausted — the step only continues past a failure once every attempt has failed.
- All attempts stream to the same step log, with a separator line (e.g. `── retry 2/3 after 30s … ──`) marking the start of each retry.

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
A post hook's stdout/stderr appears in its owning step's run log, after that step's main output (a failing post hook itself does not fail the run — it's only logged).

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

## Job Isolation: `native` and the claim pod

**Every job is isolated by default, on both agents.** An unmarked job runs
its steps inside a container — a Kubernetes Pod on the k8s-agent, and an
equivalent "claim pod" built from a pause container + one or more per-step
containers on the standard (host) agent. This is the same model on both
backends: a default (`container:`-less) step execs into the job's primary
container, `podTemplate` sidecars are reachable at `localhost` from that
step, and concurrent runs never collide because each claim gets its own
network namespace.

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata: { name: integration-test }
spec:
  podTemplate:
    spec:
      containers:
        - name: mysql
          image: mysql:8
          env: [{ name: MYSQL_ALLOW_EMPTY_PASSWORD, value: "1" }]
  steps:
    - name: test
      run: ./gradlew test          # default step: primary container, mysql on localhost:3306
    - name: dump
      container: mysql             # exec into the named sidecar
      run: mysqldump ...
```

On the standard agent, the claim pod is built lazily at claim start: a
minimal pause container (`--pause-image`, default `busybox:1.36`) owns the
network namespace; the primary container (the target of default steps) and
every `podTemplate` container join it with `--network container:<pause>`
and share the claim's workspace via a bind mount. If the `podTemplate`
defines no container, the agent injects its configured default runner image
(`--runner-image`, default `ghcr.io/eirueimi/unified-cd-runner:v0.0.3`) as
the primary. Supported container runtimes are **docker, podman, and
nerdctl** — Apple's `container` CLI is not auto-detected and not supported
for isolated jobs (no reliable network-namespace-join equivalent), so macOS
hosts must use docker/podman (typically a Linux VM) to run isolated jobs.

Sidecar containers are started eagerly and kept alive for the life of the
claim; there are **no readiness probes** — if a step connects to a sidecar
before it's ready, the step must retry/wait on its own (documented MVP
limitation, matching Kubernetes' own lack of built-in dependency ordering).
No host ports are ever published, so two concurrent claims of the same job
(or different jobs with the same sidecar image) never collide — this is the
core problem job isolation solves.

An isolated job runs every step in a Linux container regardless of the
agent's host OS, so `UNIFIED_AGENT_OS` always reports `linux` there.

### Sidecar container logs

Every user-declared `podTemplate` sidecar — every non-`job` container in
`podTemplate.spec.containers` — has its own stdout/stderr streamed into the
run's logs for the whole life of the run, on both the standard agent and the
k8s-agent. This is the sidecar's **own** process output (e.g. `mysqld`'s
startup log), not step output. The run detail UI shows it in a separate
"Sidecars" group in the step sidebar (distinct from "Steps"): one row per
sidecar, with a status dot and label — `running` while the run is live,
`exited N` once the sidecar's container terminates (`N` is its exit code).
Clicking a sidecar row filters the log view to that sidecar's own output,
same as clicking a step filters to that step.

- Only user-declared sidecars are streamed this way. The primary `job`
  container (already covered by step logs), the pause container, and the
  shim init container are not.
- A non-zero sidecar exit (`exited 1`, etc.) is shown but does **not** fail
  the run — a sidecar is a user-owned service, independent of step success.
- Sidecar logs persist in the run's log store after the pod/container is torn
  down, so a sidecar that crashed on startup can still be inspected after the
  run finishes.
- Sidecar logs are secret-masked the same way step logs are.
- On the k8s-agent, the auto-injected artifact/cache sidecar (see
  [Kubernetes Integration Guide: Artifacts and
  Cache](kubernetes-integration.md#artifacts-and-cache)) also gets its own
  entry in the Sidecars group (named `artifact`); its `exec` output used to
  be mixed into the first step's log stream and no longer is.

### `container:` — targeting a podTemplate container

Use `container:` on a step to exec into a specific `podTemplate` container
instead of the primary. This is the **canonical** way to pin a step to a
named container — it replaces the old step-level `runsIn:` field, which has
been removed:

```yaml
steps:
  - name: build
    run: go build ./...        # default: primary container

  - name: dump-db
    container: mysql           # exec into the "mysql" podTemplate container
    run: mysqldump ...
```

`container: X` requires a `podTemplate` that defines a container named `X`;
this is checked at apply time. See [Kubernetes Pod Template
(`podTemplate`)](#kubernetes-pod-template-podtemplate) below for the
container fields the standard agent understands.

> **Migrating from step-level `runsIn.image`/`runsIn.container`:** those
> forms are gone. A step-level `runsIn:` key is now a parse error with a
> migration hint. See [the migration
> guide](migration-2026-07-job-isolation.md) for the mapping to `podTemplate`
> + `container:` (or a `uses:` template — see below). The **uses-level**
> `runsIn.image` (a scope spanning an entire inlined template) is unaffected
> and still works exactly as before — see the next section.

### `native: true` — host-process jobs

Jobs that exist to use the host itself — Xcode/signing on macOS, attached
hardware, anything that isn't containerizable — opt out of isolation
entirely with `spec.native: true`. A native job runs every step as a plain
host process, exactly like today's pre-isolation behavior: no claim pod, no
`podTemplate`, no `container:` steps, no container runtime required.

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata: { name: ios-release }
spec:
  native: true                     # host processes; no container features
  agentSelector: [macos]
  steps:
    - name: build
      run: xcodebuild ...
```

Rules, enforced at apply time:

- `native: true` + `podTemplate` → error.
- `native: true` + any step `container:` → error.
- **`native` is host-only.** The k8s-agent has no concept of running outside
  a Pod. A `native: true` job is auto-routed to only the standard (host)
  agent by capability: the controller infers `requiredCaps: [native]` for it
  at trigger time, and only an agent reporting the `native` capability can
  claim it — see [Capabilities and
  routing](agents.md#capabilities-and-routing). **You do not need to
  hand-write a k8s-excluding `agentSelector` for this** on a fully-upgraded
  fleet. This capability check is skipped only for a legacy agent that
  reports no capabilities at all (pre-upgrade binary); if such an agent is a
  k8s-agent and claims a native job by label match alone, it fails the run
  immediately with a clear error as a safety net.
- Conversely, an isolated job (the default — no `native: true`) is
  auto-routed to a `container`- or `pod`-capable agent, so it lands on a
  host with a runtime or on Kubernetes. If a legacy, capability-unaware host
  agent still claims it with **no container runtime installed**
  (docker/podman/nerdctl all missing), it fails the run immediately rather
  than silently falling back to host execution — install a runtime, mark the
  job `native: true`, or upgrade the agent so capability routing keeps it
  away from runtime-less hosts.

`uses:` scope steps (below) still work inside a native job if a container
runtime happens to be present — scopes have always required a runtime
independent of the job's own isolation mode.

---

## Uses-level `runsIn.image` (scope)

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

A `uses:` step with no `runsIn` keeps its existing (non-scope) inlining
behavior — scope mode is triggered only by a **uses-level `runsIn.image`**.
`runsIn.container` on a `uses:` entry is rejected (a parse error); target a
named container from the template's own steps with `container:` instead.

**Not allowed inside a scoped `uses`** (parse errors, because they are
incompatible with holding one isolated environment across the whole template):

- a nested `runsIn:` (any form) on an inlined step — the scope is a single
  homogeneous environment, not a per-step override;
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
unified-cli artifact list <run-id>
unified-cli artifact download <run-id> <name> [--dest .]
```

```bash
# List artifacts produced by a run
unified-cli artifact list a1b2c3d4
# app-binary
# test-report

# Download and extract "app-binary" into ./out
unified-cli artifact download a1b2c3d4 app-binary --dest ./out
# extracted app-binary of run a1b2c3d4 to ./out
```

`--dest` defaults to the current directory. Both commands authenticate using the CLI's configured server token (PAT or OIDC login), the same as other `unified-cli` commands.

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

The `path`, `key`, and `restoreKeys` strings support template expressions (e.g. `path: {{ .Params.working_dir }}/node_modules`, `key: go-vendor-{{ hashFile "go.sum" }}`). A `path` or `key` that fails to expand (or expands to empty) fails the step on both agents.
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
    semaphores:
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
        in:                                # candidate resources (list, or a $param expression)
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
unified-cli run trigger build --param pool=gpu-workers
# → only agents with label "pool:gpu-workers" can claim this run
```

If `agentSelector` is omitted, any available agent can claim the run.

---

## Kubernetes Pod Template (`podTemplate`)

Defines the sidecar containers for an isolated job. On the `k8s-agent`, this
is (mostly) a real Kubernetes PodSpec. On the standard agent, the same
`podTemplate` drives the claim pod described in [Job Isolation: `native` and
the claim pod](#job-isolation-native-and-the-claim-pod) — it reads
`spec.containers` (name/image/`command`/`args`/env/`resources.limits`) to
build one network-namespace-joined container per entry. A sidecar's
`command`/`args` now match standard Kubernetes/OCI semantics on **both**
backends: `command` overrides the image's `ENTRYPOINT` and `args` overrides
its `CMD`. See [Kubernetes Integration Guide: Host container command/args
semantics](kubernetes-integration.md#host-container-commandargs-semantics)
for the full truth table and the per-runtime support matrix for the
standard agent's `--entrypoint ""` clear (docker: verified; podman,
nerdctl, wslc, Apple `container`: unverified). **On both backends**, the
primary `job` container's own image `ENTRYPOINT`/`command`/`args` are
always ignored — it is unconditionally forced to the `ucd-sh pause`
keep-alive regardless of any `command`/`args` a `podTemplate` sets on it,
so it stays alive as the exec target for `container:`-less steps. Put your
actual workload in `steps:`, not on the `job` container's `command`/`args`
— a `command` set there never runs. Sidecar containers still honor
`command`/`args` as described in the table above. Other unsupported
PodSpec fields (PVC workspace, `volumeMounts`/`securityContext`, `env`
entries without a literal `value`) are ignored with a WARN rather than
applied.

### podTemplate container parity notes (host and k8s)

The following podTemplate container behaviors are now identical on the
standard agent and the k8s-agent:

- **Primary container keep-alive (see above).** The `job` container's
  `command`/`args` are always overridden by the `ucd-sh pause` keep-alive
  on both backends — workload belongs in `steps:`.
- **`resources.requests` is host-only-ignored.** The standard agent has no
  concept of a resource *request* (only a *limit*) on docker/podman, so
  `podTemplate.spec.containers[].resources.requests` is ignored with a
  WARN (`podTemplate container resources.requests is not supported on the
  host agent ... and is ignored; use resources.limits or route to a
  Kubernetes agent`) — `resources.limits` still applies on both backends.
  Route a job that needs real CPU/memory requests to a Kubernetes agent.
- **Env `value` must be a string.** A container `env` entry's `value` must
  be a YAML string. An unquoted number or boolean (e.g. `value: 8080`) is
  a **hard error at job start on both backends** — quote it
  (`value: "8080"`). An env entry with no `value` key at all (i.e. a
  `valueFrom`-style entry, unsupported on the standard agent) is still
  only a WARN + skip on the host, not an error.
- **Every container needs a `name`.** A `podTemplate` container with no
  `name` is a **hard error at job start on both backends**
  (`podTemplate container at index N has no name`) — add a `name` to every
  entry in `spec.containers`.

**Routing is automatic and capability-based**, not selector-based: the
controller infers whether a `podTemplate` needs real Kubernetes (a named
agent-side template, an `override` patch, a pod-spec field beyond
`containers`, or a container field the host claim pod can't honor) or is
host-runnable (plain `name`/`image`/`env`/`resources.limits` containers,
`workspace.pvc` — which degrades to a host bind mount). A host-runnable
`podTemplate` can run on **either** a standard agent or a k8s-agent with no
hand-written selector required to make that work; a Kubernetes-only
`podTemplate` is routed to a k8s-agent only. See [Capabilities and
routing](agents.md#capabilities-and-routing) for the full model.

See the [Kubernetes Integration Guide](kubernetes-integration.md) for full details.

The example below uses a named agent-side template and an `override` patch,
both of which always force Kubernetes regardless of `agentSelector` — so its
`agentSelector: [kind:k8s]` is redundant here, but harmless, and documents
the intent:

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

A host-runnable `podTemplate` — no `name`, no `override`, only host-supported
container fields — needs no `agentSelector` at all; either a standard agent
(via the claim pod) or a k8s-agent can run it:

```yaml
spec:
  podTemplate:
    spec:
      containers:
        - name: job
          image: golang:1.24-alpine
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

Use `container:` in a step to target a specific container (see
[`container:` — targeting a podTemplate
container](#container--targeting-a-podtemplate-container)).

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
unified-cli approve <run-id> <step-index>
unified-cli reject  <run-id> <step-index> [--comment "reason"]
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
| `{{ secrets.NAME }}` | `env` values, `run` strings | Decrypted secret value |

> Step status is not exposed as a template variable. To branch on a step's
> outcome in an `if:` expression, use the CEL functions `failure()`,
> `success()`, or `always()` (see [Status Functions in `if:`](#status-functions-in-if)).

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
unified-cli secret set DATABASE_URL "postgres://user:pass@host/db"
unified-cli secret set API_KEY_PROD "sk-..."
unified-cli secret set slack-webhook-url "https://hooks.slack.com/services/..."
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
