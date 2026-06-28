# Agent Labels and Routing

This document covers `agentSelector` and agent-side label configuration,
which controls which agent executes a given Job.

## Table of Contents

- [Agent Labels](#agent-labels)
- [agentSelector](#agentselector)
- [Windows Agents](#windows-agents)

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
