# Migration: job-level isolation (native opt-out, claim pod)

> **Related:** a later release replaced this migration's `podTemplate →
> kubernetes`-label routing pin with capability-based routing — see
> [Migration: agent capability
> routing](migration-2026-07-agent-capability-routing.md) if you're running
> a version that includes it (agent `capabilities`, the `pod` capability,
> and the unschedulable-job warning).

This release makes **every job isolated by default, on both agents** — an
unmarked job now runs its steps inside a container (a real Kubernetes Pod on
the k8s-agent, an equivalent "claim pod" on the standard agent), instead of
running default steps directly on the host. This is a **breaking change**.
See [Job Isolation: `native` and the claim
pod](jobs.md#job-isolation-native-and-the-claim-pod) in the Job Reference for
the full model.

Two things changed at once:

1. **Isolation is now the default.** A job that used to run its steps as
   host processes on the standard agent now requires a container runtime
   (docker, podman, or nerdctl) unless it opts out with `spec.native: true`.
2. **Step-level `runsIn:` is removed.** `runsIn.image` and `runsIn.container`
   on a plain step no longer parse — `container: <name>` (targeting a
   `podTemplate` container) is now the only way to pin a step to a specific
   container. The **uses-level** `runsIn.image` (an isolated scope spanning
   an entire inlined `uses:` template) is unaffected and still works.

## Before → after

| Before | After |
|---|---|
| **Unmarked job that relies on host-process execution** (e.g. it uses tools only installed on the agent host, or was never meant to be containerized) | Add `spec.native: true` to keep running as host processes with no container features, **or** install a container runtime (docker/podman/nerdctl) on the agent so the job can run isolated as-is. If the job doesn't need anything host-specific, prefer leaving it isolated (the new default) and installing a runtime — `native: true` should be reserved for jobs that genuinely need the host (Xcode/signing, attached devices, etc.). |
| **Step-level `runsIn: { image: X }`** (fresh throwaway container per step) | Move the image to the job's `podTemplate.spec.containers` and target it with `container: <name>` on the step, **or** wrap the step in a `uses:` template with **uses-level** `runsIn: { image: X }` (a scope) if you need the old pure-function/no-shared-workspace semantics. |
| **Step-level `runsIn: { container: X }`** (exec into a named podTemplate container) | Replace with the flat `container: X` field — same target, same semantics, new field name. |
| **`runsIn.resources`** (CPU/memory requests/limits on a `runsIn.image` step) | Move to the corresponding container's `resources.limits` under `podTemplate.spec.containers[]` (Kubernetes quantity strings, e.g. `"500m"`, `"256Mi"`) — the same place k8s already reads resource limits from. `requests` is honored on the k8s-agent only; the standard agent maps `limits` only. |

## Step-by-step

### 1. Decide native vs. isolated for each existing job

For every job with no `podTemplate` today, ask: does this job need the host
itself (a specific host tool, an attached device, a macOS-only toolchain)?

- **Yes** → add `spec.native: true`. No other changes needed; the job keeps
  running exactly as it did before this release.
- **No** → do nothing to the job YAML, but make sure the agent(s) it targets
  have a container runtime installed (docker, podman, or nerdctl). Without
  one, the run now fails fast with "isolated job requires a container
  runtime (docker/podman/nerdctl); mark the job native: true or route it via
  agentSelector" instead of silently running on the host.

```yaml
# Before (implicit host execution)
apiVersion: unified-cd/v1
kind: Job
metadata: { name: ios-release }
spec:
  agentSelector: [macos]
  steps:
    - name: build
      run: xcodebuild ...

# After — this job genuinely needs the host (Xcode, signing identities)
apiVersion: unified-cd/v1
kind: Job
metadata: { name: ios-release }
spec:
  native: true
  agentSelector: [macos]
  steps:
    - name: build
      run: xcodebuild ...
```

### 2. Migrate step-level `runsIn.image`

```yaml
# Before
steps:
  - name: lint
    runsIn:
      image: golangci/golangci-lint:latest
    run: golangci-lint run ./...

# After — podTemplate container + container:
spec:
  podTemplate:
    spec:
      containers:
        - name: lint
          image: golangci/golangci-lint:latest
  steps:
    - name: lint
      container: lint
      run: golangci-lint run ./...
```

If the step relied on the old pure-function isolation (no shared job
workspace, inputs via `with:`/`env` only), use a `uses:` template with a
uses-level `runsIn.image` scope instead — see [Uses-level
`runsIn.image` (scope)](jobs.md#uses-level-runsinimage-scope).

### 3. Migrate step-level `runsIn.container`

```yaml
# Before
steps:
  - name: dump
    runsIn:
      container: mysql
    run: mysqldump ...

# After
steps:
  - name: dump
    container: mysql
    run: mysqldump ...
```

`podTemplate` itself is unchanged — only the step field name changed.

### 4. Migrate `runsIn.resources`

```yaml
# Before
steps:
  - name: build
    runsIn:
      image: golang:1.22
      resources:
        limits: { cpu: "2", memory: "2Gi" }
    run: go build ./...

# After
spec:
  podTemplate:
    spec:
      containers:
        - name: build
          image: golang:1.22
          resources:
            limits: { cpu: "2", memory: "2Gi" }
  steps:
    - name: build
      container: build
      run: go build ./...
```

## What did not change

- The **uses-level** `runsIn.image` scope (`uses:` + `runsIn: { image: X }`)
  is untouched — it's a separate code path from the removed step-level form.
- `podTemplate` on the k8s-agent is unchanged.
- Workspace carry-over (files persisting across runs of the same job)
  remains the default; only the directory layout gained a per-job
  subdirectory (`working<slot>/<job-name>` instead of `working<slot>`) — see
  [Workspace lifecycle](agents.md#workspace-lifecycle).

## Validation errors you may see after upgrading

| Error | Cause | Fix |
|---|---|---|
| `spec.native: true is incompatible with spec.podTemplate — a native job runs host processes only` | A job sets both `native: true` and `podTemplate` | Remove one — a native job has no containers at all |
| `container: requires an isolated job — remove spec.native` | A job sets `native: true` but a step also sets `container:` | Remove `native: true`, or remove the step's `container:` |
| `step-level runsIn: is no longer supported — use container: <podTemplate container name>, or move image isolation to the job's podTemplate or a uses: template` | A plain step still has `runsIn:` | See steps 2–4 above |
| `runsIn.container is not valid on a uses: step — set container: on the template's steps instead` | A `uses:` step sets `runsIn: { container: X }` | Set `container: X` on the template's own steps instead |
| `isolated job requires a container runtime (docker/podman/nerdctl); mark the job native: true or route it via agentSelector` | An isolated job was claimed by a host agent with no runtime | Install a runtime, add `native: true`, or route via `agentSelector` |
| `native: true jobs are host-only; the k8s agent cannot run them` | A `native: true` job was claimed by a k8s-agent | Route the job away from k8s-agents via `agentSelector` |
