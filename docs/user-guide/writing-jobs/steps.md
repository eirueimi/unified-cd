# Steps

## Steps

Steps are the individual execution units within a job. They run **sequentially, in the
order listed** under `steps:`. To run steps concurrently, group them inside a `parallel:`
block (see [Concurrent Steps (`parallel`)](#concurrent-steps-parallel)).

### Step names

A step's `name` must be a valid identifier matching `^[A-Za-z_][A-Za-z0-9_]*$`
(a letter or underscore, then letters, digits, or underscores). This is checked
at **apply time** — a name with a hyphen, leading digit, dot, or space fails
`apply` immediately, naming the offending step. The constraint exists because
steps are referenced in templates via dot-notation
(`{{ .Steps.<name>.Outputs.x }}`, `{{ .Steps.<name>.ChildRunID }}`) and in CEL
`if:` expressions (`steps.<name>.outputs.x`), which can only address a valid
identifier. Use underscores instead of hyphens (e.g. `build_app`, not
`build-app`). Steps inside a `parallel:` block may still be anonymous (no
`name`); the rule applies only to named steps. Note this differs from job,
container, and secret names, which follow their own (hyphen-allowing) rules.

### Shell Execution (`run`)

```yaml
steps:
  - name: build
    run: |
      go build -o bin/app ./cmd/server
      echo "Build complete"
```

- Runs in a temporary workspace directory on the agent.
- **Isolated jobs (the default — see [Job Isolation](isolation-and-containers.md#job-isolation-native-and-the-claim-pod)
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
| 4 (lowest) | System default | `["/.ucd/ucd-sh", "-c"]` for container execution (any job that isn't `native: true`); host `bash -lc` (Git Bash on Windows) for `native: true` steps — unchanged in v1 (see [Non-goals](isolation-and-containers.md#native-true-host-process-jobs)). |

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
[Configuration Reference](../../reference/configuration.md) for the `podImage`/`podTemplate`
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
mechanism](../../operator-manual/kubernetes-integration.md#step-execution-mechanism) and [Agent
Labels and Routing](../../operator-manual/agents.md) for how `/.ucd` is populated on each
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

Members of a `parallel:` group run concurrently on both agents, sharing the
run's workspace — so members that write the same path can race. A member
that must not overlap another belongs outside the block, as a plain
sequential step (steps run in declaration order by default — see above). See
[Migrating to concurrent Kubernetes step
execution](../../operator-manual/migrations/k8s-concurrent-step-execution.md)
if you're upgrading from an agent version where the Kubernetes agent ran
these one at a time.

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
zero-arg status functions (see [Status Functions in `if:`](expressions.md#status-functions-in-if)):
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
- Any failure of an attempt is retried: a non-zero exit code, an exec/infra error, or that attempt timing out. A run being cancelled stops the retry loop at the current attempt — in the main `steps` DAG. Inside [`finally:`](approval-and-finally.md#finally-block-finally) the step keeps its full attempt budget even on a cancelled run: the cancellation ends the main DAG, not the cleanup phase, so a flaky teardown still gets its retries.
- `retry:` has no overall time budget of its own, but inside `finally:` the whole cleanup phase is bounded by the agent's `finallyTimeout` (default 10m) — see [How long `finally` may run](approval-and-finally.md#how-long-finally-may-run).
- `timeoutMinutes` bounds **each attempt**, not the overall retry budget — with `attempts: 3` and `timeoutMinutes: 5`, the step can take up to 15 minutes across all tries.
- `continueOnError` is evaluated after the retry budget is exhausted — the step only continues past a failure once every attempt has failed.
- All attempts stream to the same step log, with a separator line (e.g. `── retry 2/3 after 30s … ──`) marking the start of each retry.
- On a scoped step (`runsIn.image`), `retry:` re-runs in the **same, already-mutated** scope environment across attempts — it does not get a clean scope per try; the `post:` hook binds to the final attempt's scope.
- `attempts` has no upper bound: pair it with `timeoutMinutes` (per-attempt) and `backoff` so a persistently-failing step doesn't spin in a tight re-exec loop, since retry has no overall time budget of its own.

### Post-step hooks (`post`)

Define cleanup that runs after its own pass completes (the main DAG, or — see below — `finally:`), in LIFO order within that pass.

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

A step inside [`finally:`](approval-and-finally.md) may declare a `post:` hook too. Those hooks
drain in their own pass **after** the whole `finally` block completes, so a
main-DAG step's cleanup never has to wait for `finally` to finish. Reverse
declaration order applies within each pass. Both passes run even when the run
failed, timed out, or was cancelled.

Each pass is bounded by the agent's `finallyTimeout` (default 10m) — see
[How long `finally` may run](approval-and-finally.md#how-long-finally-may-run).
If a pass hits that ceiling, the hook still running is interrupted and anything
after it in the pass is skipped: a large `cache:` save can be left unfinished.
That does not fail the run (no post-hook failure does), so the run's log records
it instead, on the run's own **System** stream:

```
unified-cd: the post:/cache: hook drain that follows the main steps did not finish: it hit the 10m cleanup budget (finallyTimeout) and was stopped. Work still in flight was interrupted and anything not yet started was skipped.
```

If you see that line on a run whose cache did not come back on the next run,
that is why — raise `finallyTimeout` for the fleet, or make the save smaller.

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

Matrix/`foreach:` combinations run concurrently within the Pod on the Kubernetes agent, the same as the standard agent — see the migration note above for jobs upgrading from an agent version where that wasn't the case.

> **Upgrade note:** matrix support changed the agent claim wire format
> (`ForeachKey`/`ForeachValue` were replaced by a `MatrixValues` map). There
> is no backward-compatibility shim — see
> [Agent Labels and Routing](../../operator-manual/agents.md#matrix-wire-format-upgrade-note) for the
> upgrade requirement.

---

