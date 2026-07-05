# Agent Labels and Routing

This document covers `agentSelector` and agent-side label configuration,
which controls which agent executes a given Job, plus the agent's workspace
lifecycle and its registration/liveness semantics with the controller.

## Table of Contents

- [Agent Labels](#agent-labels)
- [agentSelector](#agentselector)
- [Windows Agents](#windows-agents)
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
The controller uses PostgreSQL's array containment operator to check
"does the agent's label set contain all required labels" — this is an **AND match** (no OR support).

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
unified-cd run trigger build --param pool=build-arm64
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

## Workspace lifecycle

Each concurrency slot owns one workspace directory:
`<workspace-dir>/working<N>` (default `~/workspace`, override with
`--workspace-dir`, `UNIFIED_AGENT_WORKSPACE_DIR`, or the `workspaceDir`
config key). `run:` steps execute with this directory as their working
directory, and relative artifact/cache paths resolve against it.

`N` ranges from `0` to `--max-concurrent - 1`; each slot's claim loop always
uses the same `working<N>` directory for every run it executes, so
concurrent slots never share a directory.

**Workspaces are reused across runs and jobs.** Files from previous runs
remain unless the agent is started with `--clean-workspace`. If your jobs
write credentials or other secrets to disk, enable `--clean-workspace` or
delete them in a `finally:` step — otherwise later jobs on the same agent
can read them.

> **Security warning:** because workspaces persist by default, secrets
> written to disk by one Run (e.g. a checked-out credential file, a
> decrypted key) are readable by any later Run claimed into the same slot,
> even a Run from a different Job. Treat the workspace as a shared,
> semi-trusted directory unless `--clean-workspace` is enabled.

`--clean-workspace` removes and recreates `working<N>` at claim time (right
before a Run starts executing in that slot), not at agent startup — so the
very first Run after the agent starts still runs against whatever was left
in the directory from before, unless it was cleaned by a previous claim.

Every `run:` step also receives `UNIFIED_AGENT_OS` (Go's `runtime.GOOS`) in
its environment, in addition to the workspace directory as its cwd — see
[UNIFIED_AGENT_OS environment variable](#unified_agent_os-environment-variable) above.

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
