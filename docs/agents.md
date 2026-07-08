# Agent Labels and Routing

This document covers `agentSelector` and agent-side label configuration,
which controls which agent executes a given Job, plus the agent's workspace
lifecycle and its registration/liveness semantics with the controller.

## Table of Contents

- [Agent Labels](#agent-labels)
- [agentSelector](#agentselector)
- [Windows Agents](#windows-agents)
- [Kubernetes Agent](#kubernetes-agent)
- [Job isolation on the standard agent (claim pod)](#job-isolation-on-the-standard-agent-claim-pod)
- [Workspace lifecycle](#workspace-lifecycle)
- [Registration and liveness](#registration-and-liveness)
- [Matrix wire format upgrade note](#matrix-wire-format-upgrade-note)

---

## Agent Labels

Agents announce labels (tags) at startup. The controller uses them for `agentSelector` matching.

```bash
UNIFIED_AGENT_LABELS=kind:linux,pool:build ./bin/unified-cd-agent \
  --server http://localhost:8080 --token <UNIFIED_AGENT_TOKEN>

# Or via the --labels flag
./bin/unified-cd-agent --labels kind:linux,pool:build ...
```

### Automatic hostname label

When registering an agent, the controller automatically appends `hostname:<agent-hostname>`
if no explicit `hostname:*` label is present. This lets you pin a job to a specific machine
via `agentSelector` without needing to configure `--labels`.

```yaml
spec:
  agentSelector:
    - hostname:ci-worker-03
```

If the client already supplies a `hostname:*` label, that value takes precedence and
no duplicate is added.

---

## agentSelector

`spec.agentSelector` in a Job is the list of labels that a qualifying agent must have.
The controller uses PostgreSQL's `<@` (contained-by) array operator to check
`agent_selector <@ labels` — i.e. the selector must be a subset of the agent's label
set (`internal/store/postgres.go`) — this is an **AND match** (no OR support).

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
spec:
  agentSelector:
    - kind:linux
    - pool:build
  steps:
    - name: build
      run: make build
```

### Parameter expansion

Each element of `agentSelector` supports `{{ .Params.X }}` expansion using the Run's input
parameters (defined in `spec.params.inputs`). The params available at run creation time
(whether triggered via API or webhook) are used for expansion.

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
unified-cli run trigger build --param pool=build-arm64
# → agentSelector is expanded to ["pool:build-arm64"] at runtime;
#   only agents with that label can claim the run
```

> Schedule (cron) triggers do not currently pass `agentSelector` to the Run,
> so parameter expansion is not supported for scheduled runs.

---

## Windows Agents

On Windows hosts, `step.run` is executed via [Git Bash](https://git-scm.com/download/win)
(Windows does not have a native POSIX shell).

### Git Bash not found

At agent startup, the PATH and known install locations (e.g. `C:\Program Files\Git\bin\bash.exe`)
are searched. If Git Bash is not found, **the agent exits with an error at startup**
(previously the failure was only discovered at the first step execution).

```
shell check failed error="git bash not found — install Git for Windows (https://git-scm.com/download/win) or add bash.exe to PATH"
```

Fix: Install [Git for Windows](https://git-scm.com/download/win) or add `bash.exe` to your PATH.

### UNIFIED_AGENT_OS environment variable

Every `step.run` receives the `UNIFIED_AGENT_OS` environment variable (Go's `runtime.GOOS`:
`windows` / `linux` / `darwin`). Job authors can use this to branch on OS.

> **Isolated jobs and scopes always run in Linux, regardless of host OS.** A
> step in an isolated job (the default — see [Job Isolation: `native` and the
> claim pod](jobs.md#job-isolation-native-and-the-claim-pod)) or a `uses:`
> scope step always executes in a Linux container, so `UNIFIED_AGENT_OS` is
> `linux` there even when the agent's host is Windows or macOS
> (`internal/agent/agent_os.go: agentOSForStep`). Only steps in a `native:
> true` job run directly on the host and report the host's `runtime.GOOS`.

```yaml
steps:
  - name: platform-specific
    run: |
      if [ "$UNIFIED_AGENT_OS" = "windows" ]; then
        echo "Running in Git Bash on Windows"
      else
        echo "Running in native Unix shell"
      fi
```

---

## Kubernetes Agent

The sections above are host-agent-centric; the k8s-agent participates in the same
`agentSelector` routing with a few Kubernetes-specific differences:

- It auto-registers a `kubernetes` label in addition to any configured `labels`
  (`internal/k8sagent/agent.go`), so `agentSelector` can target k8s pools without
  extra configuration.
- It supports `call:` steps like the standard agent.
- It runs job/scope steps as Pods and attaches an artifact sidecar
  (`unified-artifact`) for `uploadArtifact`/`downloadArtifact`/`cache` steps.

See [docs/kubernetes-integration.md](kubernetes-integration.md) for full setup,
sidecar, and pod-lifecycle details.

---

## Job isolation on the standard agent (claim pod)

By default (unless a job sets `spec.native: true`), the standard agent runs
a claim's steps inside a per-claim "claim pod": a pause container that owns
a network namespace, plus one container per `podTemplate.spec.containers`
entry (and an injected primary container if none is defined), all joined to
the pause container's netns and sharing the claim's per-job workspace via a
bind mount. This mirrors the Kubernetes agent's real Pod: `podTemplate`
sidecars are reachable at `localhost` from a default step, and no host ports
are ever published, so concurrent claims never collide. See [Job Isolation:
`native` and the claim pod](jobs.md#job-isolation-native-and-the-claim-pod)
in the Job Reference for the full model and YAML examples.

**Supported container runtimes:** docker, podman, nerdctl. Apple's
`container` CLI is **not** supported for isolated jobs — there is no
reliable `--network container:<id>` equivalent for it, so it remains usable
only for whatever it was already used for (it is not used by the claim
pod). macOS hosts run isolated jobs via docker or podman.

**Agent flags/config for the claim pod:**

| Flag | Config key | Default | Purpose |
|---|---|---|---|
| `--pause-image` | `pauseImage` | `busybox:1.36` | Image for the pause (netns-holder) container, one per claim. |
| `--runner-image` | `runnerImage` | `ghcr.io/eirueimi/unified-cd-runner:v0.0.3` | Primary container image injected when the job's `podTemplate` defines none. |

See [Configuration Reference: Agent Flags](configuration.md#agent-flags) for
the full flag list.

### Troubleshooting isolated claims

If a claim fails before any step runs — no container runtime found, the
claim pod failed to start (e.g. image pull failure), or workspace
preparation failed — the agent fails the run immediately and writes the
reason as a **"System"** line in the run's own logs (internally `stepIndex
-1`, shown in the Web UI/CLI logs as a system-level entry rather than
attached to any step). Check the run's log output first, even if no step
appears to have started; the actionable error (e.g. "isolated job requires a
container runtime (docker/podman/nerdctl); mark the job native: true or
route it via agentSelector") is there, not just in the agent's own process
log.

---

## Workspace lifecycle

Each concurrency slot owns one slot directory:
`<workspace-dir>/working<N>` (default `~/workspace`, override with
`--workspace-dir`, `UNIFIED_AGENT_WORKSPACE_DIR`, or the `workspaceDir`
config key). Within that slot directory, **every job gets its own
subdirectory**, named after a sanitized form of the job's `metadata.name`:
`working<N>/<sanitized-job-name>`. `run:` steps execute with this per-job
directory as their working directory (bind-mounted at `/workspace` — or
`podTemplate.workspace.mountPath` — into every claim-pod container for an
isolated job), and relative artifact/cache paths resolve against it.

`N` ranges from `0` to `--max-concurrent - 1`; each slot's claim loop always
uses the same `working<N>/<job>` directory for every run of that job, so
concurrent slots never share a directory, and — because the subdirectory is
per-job — two different jobs sharing a slot over time never mix files either
(this also closes the pre-isolation cross-job file mixing within a slot).

**Workspaces are reused (carry over) across runs of the same job.** This is
the default and is unchanged by job isolation: files from a job's previous
run remain in its per-job directory unless cleaned. Two knobs control
cleaning, and they OR together (either one triggers a clean at claim start):

- **Agent-level `--clean-workspace`** (`UNIFIED_AGENT_WORKSPACE_DIR` sibling
  flag) — wipes every job's directory on every claim, agent-wide.
- **Job-level `podTemplate.cleanWorkspace: true`** — wipes only that job's
  directory, on every claim of that job, on **both** agents (the host agent
  now honors the same per-job knob the k8s-agent pool already did).

If your jobs write credentials or other secrets to disk and you don't use
either cleaning knob, delete them in a `finally:` step — otherwise later runs
of the same job (or, without per-job isolation this no longer applies across
*different* jobs) can still read files left in a stale carry-over directory.

> **Security note:** because workspaces persist by default, secrets written
> to disk by one Run of a job are readable by a later Run of **that same
> job** claimed into the same slot, unless cleaning is enabled. Per-job
> directories mean this no longer crosses job boundaries.

**Mode marker (`.ucd-mode`).** Each per-job directory carries a
`.ucd-mode` file recording whether the job last ran `native` or isolated. If
a job's definition flips modes between runs (e.g. `native: true` added or
removed), the directory is reset before the next claim — this closes the one
remaining root-ownership leftover hole described below.

**Root-owned files (Linux, rootful docker).** Containers created by rootful
docker write as root inside the bind-mounted workspace; the agent process
(usually non-root) can then fail to clean or overwrite those files
(`EPERM`). When a plain `os.RemoveAll` clean fails this way, the agent falls
back to a throwaway root cleanup container (`<runtime> run --rm -v
<workDir>:/w busybox rm -rf /w/. ...`) before recreating the directory; if
that also fails, it WARNs and proceeds with whatever is left (the previous,
pre-isolation behavior). **Recommended fix: run rootless podman.** With
rootless podman, the container's root maps to the agent's own user, so this
class of problem does not occur in the first place — it is the first-choice
deployment for Linux hosts running isolated jobs.

**Disk usage is an operator responsibility.** Per-job directories accumulate
under each `working<N>/` over time (one subdirectory per distinct job name
ever run in that slot); there is no automatic workspace GC. Include
`wsBase` in your normal disk-usage monitoring/cleanup.

**macOS/Windows file-sharing requirement.** For isolated jobs, `wsBase` (the
`--workspace-dir` root) must live under a path the container runtime's file
sharing exposes to containers — e.g. under `/Users` for Docker Desktop on
macOS, or an equivalent shared drive on Windows. A `wsBase` outside the
runtime's shared paths will fail to bind-mount into the claim pod.

`--clean-workspace` removes and recreates a job's directory at claim time
(right before a Run starts executing in that slot), not at agent startup —
so the very first Run of a job after the agent starts still runs against
whatever was left in the directory from before, unless it was cleaned by a
previous claim.

Every `run:` step also receives `UNIFIED_AGENT_OS` (Go's `runtime.GOOS` on
the host for a `native: true` job, but `linux` for an isolated job or a
`uses:` scope regardless of host OS) in its environment, in addition to the
workspace directory as its cwd — see [UNIFIED_AGENT_OS environment
variable](#unified_agent_os-environment-variable) above.

---

## Registration and liveness

On startup, an agent registers with the controller (`POST
/api/v1/agents/register`), sending its ID, hostname, OS, labels, version, and
exposed environment variables. **Registration replaces the stored label set
wholesale** — if a label from a prior registration is absent from the new
`--labels`/`UNIFIED_AGENT_LABELS`/config value, it is dropped. This means
removing a label and restarting the agent actually takes effect, rather than
labels only ever accumulating.

When an agent claims a run (`POST /api/v1/agents/{id}/claim`), the
controller also upserts the agent row, but via a separate, non-destructive
path: it only overwrites hostname/OS/version/env when the claim supplies a
non-empty value, and it **merges** (rather than replaces) labels. This lets
a claim refresh `last_seen_at` and pick up newly-observed labels without
clobbering richer data recorded at full registration time (e.g. the
register-only `hostname:<h>` label — see [Automatic hostname
label](#automatic-hostname-label)).

Independent of claim activity, the agent runs a background heartbeat loop
that calls `POST /api/v1/agents/{id}/heartbeat` every 15s
(`agent.DefaultHeartbeatInterval`), refreshing `last_seen_at`. This matters
because a busy agent (all concurrency slots occupied) stops polling for new
claims, and without a heartbeat it could be misidentified as dead. See
[High Availability: Orphaned-Run Recovery](high-availability.md#orphaned-run-recovery)
for how the controller uses `last_seen_at` staleness to reap stuck runs and
delete dead agent rows — that mechanism is documented there, not duplicated
here.

---

## Matrix wire format upgrade note

The release that added `matrix:`/`foreach:` step expansion (see
[docs/jobs.md: Matrix and Foreach Steps](jobs.md#matrix-and-foreach-steps))
changed the claim wire format: the previous per-claim `ForeachKey` /
`ForeachValue` string fields were replaced by a `MatrixValues map[string]string`
field (one entry per matrix dimension; a foreach-sugared step produces a
single-entry map).

**There is no backward-compatibility shim for this change.** An agent
(standard or k8s-agent) running the pre-matrix binary cannot correctly
expand `foreach:`/`matrix:` steps claimed under the new wire format —
**the controller and every agent (standard and k8s-agent alike) must be
upgraded together.** Do not roll out a matrix-capable controller while
older agent binaries are still claiming runs from it.
