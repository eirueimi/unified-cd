# Steps and Execution

## Run fails with `dynamic secret name must be resolved from a parameter before execution`

**Symptom**

A run fails before any step starts with:

```
dynamic secret name must be resolved from a parameter before execution
```

**Cause**

The job selects a secret name from a runtime value such as `.Steps`, `.Matrix`,
or `.Foreach`. The controller must know the literal secret names before an
agent claims the run so it can authorize only those secrets.

**Fix**

Pass a literal secret name through a Job parameter, a JobTemplate default, or
`uses.with`, then reference it as `{{ index .Secrets .Params.token_secret }}`.
Do not derive the secret name from a normal step output or another runtime
value.
Do not work around the validation by assigning `.Secrets` to a variable or
passing it through `or`, `and`, `with`, `range`, a pipeline, or a named
template. Rewrite the job so the secret name is a literal `with:` value that
feeds the exact `index .Secrets .Params.NAME` form.

## Job isolation

Jobs are isolated by default (see [Job Isolation: `native` and the claim
pod](../user-guide/writing-jobs/isolation-and-containers.md#job-isolation-native-and-the-claim-pod)); most of the failures below are an
isolation setup gap surfacing as a run failure.

### Run fails immediately: "isolated job requires a container runtime"

**Symptom**

A run fails immediately — no step ever starts — with a **System** log line (`stepIndex -1`):

```
isolated job requires a container runtime (docker/podman/nerdctl); mark the job native: true or route it via agentSelector
```

**Cause**

The job is isolated (no `spec.native: true`) and was claimed by a standard agent whose host has
none of docker, podman, or nerdctl installed. An isolated job needs a container runtime to build
its claim pod; without one, the agent fails the run immediately instead of silently running the
steps on the host (`internal/agent/agent.go`).

**Fix**

- Install docker, podman, or nerdctl on the agent host, or
- Add `spec.native: true` to the job if it's meant to run as host processes, or
- Route the job to an agent that has a runtime via `agentSelector`.

### Run fails immediately on Kubernetes: "native: true jobs are host-only"

**Symptom**

A run fails immediately with:

```
native: true jobs are host-only; the k8s agent cannot run them
```

**Cause**

The job sets `spec.native: true`, but it was claimed by a k8s-agent. `native` opts a job out of
containerization entirely, and the k8s-agent has no concept of running outside a Pod, so it
cannot honor that (`internal/k8sagent/agent.go`).

**Fix**

Route `native: true` jobs away from k8s-agents (and toward host agents with the tools the job
needs) via `agentSelector`.

### Workspace cleaning warnings after a job flips native ↔ isolated

**Symptom**

The agent log shows `workspace clean failed; retrying via cleanup container` and/or `cleanup
container failed; proceeding with dirty workspace`, often right after a job's `native: true` was
added or removed.

**Cause**

Each per-job workspace directory carries a `.ucd-mode` marker recording whether the job last ran
native or isolated; when a job's mode flips, the agent resets the directory before the next claim
(`internal/agent/workspace.go`). This is also where root-owned leftovers can appear: containers
created by **rootful docker** write files as root inside the bind-mounted workspace, which the
agent's own process can't remove. The agent retries via a throwaway root cleanup container; if
that also fails, it **WARNs** and proceeds with whatever is left rather than failing the run.

**Fix**

- Run **rootless podman** on the agent host — the container's root maps to the agent's own user,
  so root-owned leftovers don't occur in the first place.
- If you see the WARN with rootful docker, manually clean the affected per-job workspace
  directory with elevated permissions — see [Workspace lifecycle](../operator-manual/agents.md#workspace-lifecycle).

### Stray `ucd-sh pause` containers on an agent host after an agent crash

**Symptom**

`docker ps` (or `podman ps`) on an agent host shows pause and/or sidecar containers still running
`/.ucd/ucd-sh pause` long after the runs that created them finished — typically noticed after the
agent process was killed, OOM'd, or the host rebooted. (Older agent versions ran `sleep infinity`
instead — same symptom, different command.)

**Cause**

This is expected, not a bug. Claim pod containers are long-lived (`/.ucd/ucd-sh pause`, not
`--rm`) and are torn down by the agent itself when a claim finishes; if the agent exits ungracefully
mid-claim, that teardown never runs. Unlike the k8s-agent, whose orphaned pods are eventually
reaped by the cluster's own pod garbage collection, **the host agent has no automatic container
GC** — see [Crash-orphaned claim containers](../operator-manual/agents.md#crash-orphaned-claim-containers).

**Fix**

Treat this as routine hygiene: periodically prune claim-pod-shaped containers on agent hosts
(e.g. a `docker container prune`-style sweep, or one scoped to containers made from the
`pauseImage`/`runnerImage`/podTemplate images), rather than assuming a crash cleans up after
itself.

---

## Conditional step ran when it shouldn't

**Symptom**

A step gated with `if:` runs even though its condition looks false, and the
**run's own log** contains a `System` line like:

```
unified-cd: step "deploy": if: expression "{{ eq .Params.x \"y\" }}" compile error: ERROR: <input>:1:17: Syntax error: missing ':' at '"y"' — the condition could not be evaluated, so the step RAN (fail-safe)
```

The agent log carries the same reason as `if: condition eval failed, running
step` (both agents share the orchestrator, so the line is identical on the
host and Kubernetes agents).

**Cause**

`if:` expressions are **CEL**, not Go templates — unlike `run:`, `env:`, and
`outputs:` in the same job, which do use `{{ .Params.X }}`-style Go template
syntax. An expression that fails to compile or evaluate **fails open**: the
step still runs, and the run is not marked failed.

Most of these are now caught earlier: `unified-cd apply` compiles every `if:`
against the same environment the agent uses and rejects the job with
`if: expression "..." does not compile`. A condition can still fail at run
time when it comes from a `uses:` template (resolved after apply), or when it
compiles but errors during evaluation — the commonest cause being a
**`params` key the job does not declare**, which raises `no such key`.

**Fix**

- Use valid CEL syntax, with lowercase variables and no `{{ }}` delimiters:
  ```yaml
  if: 'params.env == "production"'
  ```
  not:
  ```yaml
  if: '{{ eq .Params.env "production" }}'   # wrong — Go template, rejected at apply
  ```
- Reference only parameters declared in `spec.params.inputs`; an undefined
  `params.X` errors and fails open. (An undefined `vars.X` does **not** — it
  reads as the empty string, so the gate stays shut, and the run's log gets a
  `System` line naming the key.)
- After a run, check the run's log for `System` lines beginning
  `unified-cd: step "..."` — that is where a condition that did not mean what
  it says now reports itself.
- See [Job Reference: Conditional Execution (`if`)](../user-guide/writing-jobs/steps.md#conditional-execution-if)
  for the full CEL variable/function reference — this is especially important
  to verify for any `if:` gating a production deploy.

## A step's log shows `step panicked: ...`

**Symptom**

A step fails with a log line like:

```
step panicked: runtime error: invalid memory address or nil pointer dereference
```

(stream `stderr`), and the run is `Failed` (or, for a `continueOnError: true`
step, the step itself is `Failed` but the run continues).

**Cause**

Something inside the step's own execution path — the step body, template
expansion, or the underlying backend exec — panicked instead of returning a
normal error. The agent recovers the panic at the step boundary, writes the
panic value and stack into that step's own log (this line), and reports the
step `Failed`, honoring `continueOnError` exactly like a normal error would.
Only this run is affected: sibling concurrent runs on the same agent and the
agent process itself are unaffected — a panic here used to crash the whole
agent process (taking down every run it was executing), which is why this is
now caught at the step boundary rather than left to propagate.

**Fix**

Treat it like any other step failure: the panic message and stack trace in
the step's log point at the failing code path (a job's `run:` script, a
custom tool, or the like). Fix the underlying bug in whatever the step
invoked; there is no agent-side workaround needed once the run itself is
correctly reported `Failed` rather than stuck `Running`.

A rarer variant panics outside the step body itself — e.g. while preparing
the workspace, before any step started — and surfaces instead as the run
being failed with an `agent panic: ...` message (no per-step log line, since
no step ever ran). The cause and fix are the same: something panicked, the
agent turned it into a normal Failed outcome instead of crashing, and the
panic text points at the failing code.

---

## A `finally` step fails with `context deadline exceeded` and the run is `Failed`

**Symptom**

A step inside `spec.finally` is reported `Failed` after roughly ten minutes,
its log carries a line like

```
unified-cd: step "cleanup" failed to execute: context deadline exceeded
```

and the run's **System** stream carries

```
unified-cd: the finally: phase did not finish: it hit the 10m cleanup budget (finallyTimeout) and was stopped. Work still in flight was interrupted and anything not yet started was skipped.
```

The run finishes `Failed` — including runs a user had cancelled, which
previously finished `Cancelled`, and including a `finally` block whose steps
are all `continueOnError: true`, because a phase that did not finish is not a
step's fact to suppress.

**The same symptom with no step involved.** If the System line names a hook
drain instead —

```
unified-cd: the post:/cache: hook drain that follows the main steps did not finish: it hit the 10m cleanup budget (finallyTimeout) and was stopped. Work still in flight was interrupted and anything not yet started was skipped. Interrupted: the cache: save for step "deps" (key "go-mod-linux-9f3c"). Never started: the post: hook of step "integration".
```

— then a `post:`/`cache:` hook was cut off and the run's status is **not**
affected: a successful job still reports `Succeeded`. That is the case to look
for when a `cache:` save silently did not land and the next run got a cache
miss. Post-hook failures have never changed a run's status; the System line is
the record.

Read the `Interrupted:` / `Never started:` tail to find **which** cache or
hook: `Interrupted:` names the one item the deadline landed inside (at most
one), `Never started:` the queued items behind it, summarised as `(+N more)`
past five so a job with hundreds of hooks still yields one line. Items named
there are the ones whose side effects did not happen — those caches are stale,
those hooks did not run.

**Cause**

The cleanup phase now has a ceiling. `finally` deliberately ignores run
cancellation, and that also discards the job-level `spec.timeoutMinutes`
deadline, so the agent applies `finallyTimeout` (default **10m**) to each
cleanup phase instead — each `post:`/`cache:` hook drain, the `finally`
pipeline, and scope/claim-pod teardown — plus, on the Kubernetes agent, a
fifth window for the claim Pod's own deletion or return to the idle pool,
which happens after the shared cleanup loop returns. So a run's worst-case
post-DAG time is 5 × `finallyTimeout` (50m at the default) on the Kubernetes
agent and 4 × (40m) on the standard agent; size anything against the larger.
Before this existed, such a step ran forever: it pinned the run, held
one of the agent's concurrency slots, and nothing detected it, because the
stuck-run reaper keys on **agent** liveness and the agent keeps heartbeating.
The most common way to hit the wall is a `call:` in `finally` with no
`timeoutMinutes:`, waiting on a child run that will never be claimed.

**Fix**

- Set `timeoutMinutes:` on the cleanup step itself. It works inside `finally`
  and is the precise control; the fleet-wide budget is only a backstop.
- If the cleanup genuinely needs longer (a large cache save, a slow rollback),
  raise the agent's budget: `--finally-timeout` /
  `UNIFIED_AGENT_FINALLY_TIMEOUT` / `finallyTimeout` on the standard agent,
  `finallyTimeout` / `UNIFIED_K8S_FINALLY_TIMEOUT` on the Kubernetes agent. A
  non-positive value does **not** mean "unbounded" — it falls back to the 10m
  default; an unparseable one is a startup error (except in
  `UNIFIED_AGENT_FINALLY_TIMEOUT`, which is ignored when it does not parse).
- If the step is hanging rather than slow, the deadline is telling you the
  truth: fix what it is waiting on. See
  [Bounding a run's cleanup phase](../operator-manual/agents.md#bounding-a-runs-cleanup-phase-finallytimeout).

---

## A cancelled run finished `Failed` instead of `Cancelled`

**Symptom**

A user cancelled a run; the run's terminal status is `Failed`, and a step
inside `spec.finally` is reported `Failed`.

**Cause**

This is correct, and is a deliberate change. A `finally` step that exits
non-zero failed for its own reasons — its context was never cancelled, since
the whole point of `finally` is that it runs after a cancellation — so the
failure is reported as a failure and the run is marked `Failed`. Previously
the step was relabelled `Cancelled` and the failure was discarded, which
showed operators a clean cancellation over broken teardown.

**Fix**

Read the failing `finally` step's log and fix the cleanup. If the run should
have finished `Cancelled`, the cleanup must succeed — a `finally` block whose
steps all succeed still leaves a cancelled run `Cancelled`. See
[Approval and Finally](../user-guide/writing-jobs/approval-and-finally.md#finally-block-finally).

