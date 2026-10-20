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

### Run fails with `prepare workspace failed: clean workspace ...`

**Symptom**

A run fails before its first step, and the run's own log (and the agent log) carries:

```
prepare workspace failed: clean workspace /data/ws/working0/my-job: ... (cleanup container: ...)
```

with a preceding agent-log WARN `workspace clean failed; retrying via cleanup container`. On the
Kubernetes agent, a run on a **reused** Pod (`podTemplate.reuse: true`) fails with:

```
k8s: could not clear the reused Pod's workspace: ...
```

**Cause**

Every ordinary workspace is emptied before every run, so a run never starts on another run's
files. When emptying fails the run fails too: it used to warn and proceed on the dirty directory.
The usual cause is **root-owned files** that a rootful docker container wrote into the
bind-mounted workspace, which the (usually non-root) agent process cannot remove. The agent retries
through a throwaway root cleanup container (a `busybox` container with the directory mounted), and
the run fails only if that also fails (no container runtime reachable, the image cannot be pulled,
the directory is on a filesystem the container cannot see). A reused Kubernetes Pod whose
workspace cannot be cleared is deleted rather than returned to the pool.

(The `.ucd-mode` marker file that earlier agents kept in every workspace, and the reset of the
directory when a job flipped between `native` and isolated, no longer exist for ordinary
workspaces. A volume used as the workspace is different: see [Run fails: volume was last used by
a native/isolated run](#run-fails-volume-was-last-used-by-a-nativeisolated-run).)

**Fix**

- Run **rootless podman** on the agent host: the container's root maps to the agent's own user, so
  root-owned leftovers don't occur in the first place.
- With rootful docker, delete the affected `working<N>/<job>` directory by hand with elevated
  permissions and re-trigger the run, and make sure the agent host can pull `busybox` or has it
  cached. See [Workspace lifecycle](../operator-manual/agents.md#workspace-lifecycle).

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

## A run waits for a volume another run holds

**Symptom**

A run that mounts a [named volume](../user-guide/writing-jobs/volumes.md) is claimed but runs no
steps. Its own log (the run-level "System" stream) has one line, and the agent holds the run in a
concurrency slot until it ends:

```
waiting for volumes ["unity-main"] and paths [], which another run on this agent is using
```

On a Kubernetes agent the line is:

```
waiting for volumes [["unity-main"]], which another run is using
```

**Cause**

An `exclusive` volume is held by one run at a time, and a native job's absolute `mountPath` is held
agent-wide, whatever the volume's mode. The controller avoids handing an agent a run for a volume it
reports busy, but it cannot be exact, so a claimed run can still wait: it holds a slot, and nothing
is wrong. It starts when the holder ends. Cancelling the waiting run frees its slot; so does the
agent draining, which fails the waiting run with `run abandoned: the agent is draining and the run
was still waiting for its volumes`.

**Fix**

- Wait, or add copies: declare several volumes and list them as candidates in the job
  (`name: [go-cache-1, go-cache-2]` with `as:`), so concurrent runs each get a free one.
- If the volume is only a cache, declare it `shared`.
- A native run with an **absolute** `mountPath` waits for the path even when copies are free:
  candidate lists help a native run only for relative mount paths.

## A volume stays held after its run crashed

**Symptom**

On a Kubernetes agent, runs that mount an `exclusive` volume log `waiting for volumes [[...]], which
another run is using` for a few minutes after the run that held it crashed or its Pod was deleted,
and then proceed on their own. The PVC carries the annotation `unified-cd/volume-held-by: <run id>`.

**Cause**

The holder's run ID is recorded on the PVC before its Pod is created and cleared once the Pod has
been deleted. A holder that dies without clearing it blocks the volume until the lock is judged
stale: the lock is taken over once its run has **no live Pod**, which takes the two-minute grace
that protects a lock whose Pod is still being created plus the Pod garbage-collection cadence.
**Expect up to about three minutes.**

**Fix**

Nothing: it clears itself. Do not remove the annotation by hand while a Pod of that run still
exists. The takeover is logged as `k8s: taking over a volume lock whose run has no live pod`.

## Run fails: volume was last used by a native/isolated run

**Symptom**

A run that uses a volume as its workspace (`mountPath: "."`) on a host agent fails with:

```
prepare workspace failed: volume "unity-main" was last used by a native run and this run is isolated: the agent will not reset a volume itself, because that would delete its contents. To keep the data, run the job as native again. To wipe the volume once and continue as isolated, set spec.cleanWorkspace: true on the job. The agent records the mode in <wsBase>/.ucd-volume-modes/unity-main; deleting that file also lets the run proceed without wiping
```

**Cause**

The agent records, in its own state directory and never inside the volume, whether each
workspace volume was last used by a `native` or an isolated run. Ordinary workspaces are simply
recreated; a volume is never reset by the agent because that would delete up to tens of
gigabytes. So a mode flip fails the run and names the three ways out.

**Fix**

- Run the job in the mode the volume was last used in (undo the `spec.native` change), or
- set `spec.cleanWorkspace: true` for **one** run to wipe the volume and continue in the new mode,
  then remove it (leaving it set wipes the volume on every run), or
- delete the marker file named in the message, to adopt the volume as it is. Do this when you know
  the contents are compatible with the new mode; a rootful container leaves root-owned files that
  break a later native run.

## Run fails: `already exists and was not created by this agent as a volume link`

**Symptom**

A `native: true` run fails before its first step:

```
volume mount failed: /home/ci/.cache/go already exists and was not created by this agent as a volume link, so it will not be replaced; remove or rename it, or choose a different mountPath
```

**Cause**

On a native run a volume is realised as a symlink, and an *absolute* `mountPath` is a symlink at
that host path. unified-cd never moves or deletes what it did not create, so if a real file or
directory sits there (for example the toolchain's own cache directory from before you mounted a
volume there) the run fails instead of replacing it. A stale link of the agent's own is
replaced silently; this message means the path holds something else.

**Fix**

Remove or rename the existing path on the agent host, or mount the volume at a different
`mountPath` (a relative one lives inside the workspace and cannot collide with anything).

## Run fails: `cannot link ... on Windows`

**Symptom**

A `native: true` run on a Windows agent fails:

```
volume mount failed: cannot link <link> to <target> on Windows: creating a symlink failed (...; it needs administrator rights or Developer Mode) and so did creating a directory junction (...; it needs an NTFS volume and a path without cmd.exe metacharacters)
```

**Cause**

A native volume mount is a symlink. On Windows, creating one needs administrator rights or
Developer Mode; the agent falls back to a directory junction, which needs an NTFS volume and a
path with no `cmd.exe` metacharacters (such as `&`, `^`, `%`). Both failed.

**Fix**

Enable Developer Mode (or run the agent with the symlink privilege), or put the volume's `path`
and the workspace on an NTFS volume and rename any directory containing a metacharacter. The error
carries both underlying causes; read them.

## Run fails: volume is shared and cannot be mounted at the workspace root

**Symptom**

```
spec.volumes[0]: volume "go-cache" is shared and cannot be mounted at the workspace root (mountPath "."): several runs would work in one checkout with no exclusion. Declare it exclusive in the agent config, or mount it at a subdirectory
```

(prefixed `volume mount failed: ` on a host agent, `k8s: ` on the Kubernetes agent).

**Cause**

`mode` lives in the agent's config, so apply cannot know it; the same job YAML is valid against an
agent that declares the volume `exclusive`. Several runs sharing one working tree would overwrite
each other.

**Fix**

Declare the volume `exclusive` in the agent's config and restart the agent, or mount it at a
subdirectory (`mountPath: .ucd-cache/go`), or route the job to an agent that declares it
exclusive.

## A shared volume never binds (RWX unavailable)

**Symptom**

On a Kubernetes agent, a run that mounts a `mode: shared` volume fails after the pod start
timeout (default 5 minutes):

```
k8s: run pod did not become ready: waiting for Pod ucd-run-... to become Running: context deadline exceeded ...
```

and the volume's PVC (`<agentName>-<volume>`) is stuck `Pending`.

**Cause**

`mode: shared` requests a `ReadWriteMany` PVC, which only binds on a StorageClass that supports
RWX: in practice a network filesystem (NFS, CephFS, EFS, Azure Files, Longhorn). The agent creates
the PVC at startup regardless; the cluster cannot bind it, so every Pod that mounts it stays
`Pending`.

**Fix**

- `kubectl -n <ns> describe pvc <agentName>-<volume>` shows the provisioner's reason.
- Set `storageClassName` on the volume to an RWX-capable class. The agent **does not change an
  existing PVC** (it logs a mismatch warning): delete the Pending PVC, then restart the agent so it
  is recreated.
- Or declare several `exclusive` volumes and list them as candidates in the job; the agent picks a
  free one. That gives the same parallelism without RWX, and a local-disk StorageClass is faster
  than a network filesystem for a Go module cache or a Unity `Library/`.

## Permission denied writing to a volume on Kubernetes

**Symptom**

On a Kubernetes agent, a step whose container runs as a non-root user fails writing into a mounted
volume, while the same job works on a host agent:

```
mkdir: cannot create directory '/workspace/.ucd-cache/go/build': Permission denied
```

**Cause**

A freshly provisioned PVC is usually owned by root, and many storage provisioners do not change
that. A container that runs as a non-root user cannot write to it. (On a host agent the volume is a
directory the agent user created, so it does not arise.)

**Fix**

Set `securityContext.fsGroup` in the job's `podTemplate.spec` (or the agent's named template) to the
container user's group, so Kubernetes makes the volume group-writable when it mounts it:

```yaml
podTemplate:
  spec:
    securityContext:
      fsGroup: 1000
    containers:
      - name: job
        image: my-builder:latest
        securityContext:
          runAsUser: 1000
```

`securityContext` is a Kubernetes-only field, so a job that sets it is pinned to a Kubernetes
agent.

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

A *malformed* expression is caught earlier: `unified-cd apply` compiles every
`if:` against the same environment the agent uses and rejects the job with
`if: expression "..." does not compile`. What apply cannot catch is an
expression that compiles and then errors during evaluation — the commonest
cause being a **misspelt `params` key**, which raises `no such key` — or one
that only appears at run time because it came from a `uses:` template
(resolved after apply).

**Fix**

- Use valid CEL syntax, with lowercase variables and no `{{ }}` delimiters:
  ```yaml
  if: 'params.env == "production"'
  ```
  not:
  ```yaml
  if: '{{ eq .Params.env "production" }}'   # wrong — Go template, rejected at apply
  ```
- Check the spelling of every `params` name by hand. An undefined `params.X`
  errors and fails open, and nothing validates the name at apply time —
  a param does not have to be declared under `spec.params.inputs` to reach a
  run (`--param`, a webhook `paramsMapping`, a schedule, and a `call:` step's
  `with:` all pass undeclared keys through), so a typo cannot be told apart
  from a legitimate pass-through reference. An undefined `vars.X` does **not**
  behave this way — it reads as the empty string, so the gate stays shut, and
  the run's log gets a `System` line naming the key.
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

