# Isolation and Containers

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
  Cache](../../operator-manual/kubernetes-integration.md#artifacts-and-cache)) also gets its own
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

> **Step-level `runsIn.image`/`runsIn.container` are removed.** A step-level
> `runsIn:` key is now a parse error with a hint to use `podTemplate` +
> `container:` (or a `uses:` template — see [Uses-level `runsIn.image`
> (scope)](templates-and-reuse.md#uses-level-runsinimage-scope)). The **uses-level**
> `runsIn.image` (a scope spanning an entire inlined template) is unaffected
> and still works exactly as before — see [`native: true`](#native-true-host-process-jobs).

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
  routing](../../operator-manual/agents.md#capabilities-and-routing). **You do not need to
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
semantics](../../operator-manual/kubernetes-integration.md#host-container-commandargs-semantics)
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
PodSpec fields (`volumeMounts`/`securityContext` and any other container
field outside the host-supported set) already require a Kubernetes agent —
the run is pinned there rather than degraded on the standard agent. A PVC
workspace is the one exception that still degrades instead of routing: it
becomes a per-claim bind mount on the standard agent, by design. An `env`
entry without a literal `value` (i.e. `valueFrom`) also now requires a
Kubernetes agent rather than being silently dropped — see [Kubernetes
Integration Guide: the remaining intentional
differences](../../operator-manual/kubernetes-integration.md) and the
[migration
guide](../../operator-manual/migrations/podtemplate-subfield-routing.md).

### podTemplate container parity notes (host and k8s)

The following podTemplate container behaviors are now identical on the
standard agent and the k8s-agent:

- **Primary container keep-alive (see above).** The `job` container's
  `command`/`args` are always overridden by the `ucd-sh pause` keep-alive
  on both backends — workload belongs in `steps:`.
- **`resources.requests` requires a Kubernetes agent.** The standard agent
  has no concept of a resource *request* (only a *limit*) on docker/podman.
  A `podTemplate` container that sets
  `podTemplate.spec.containers[].resources.requests` now requires a
  Kubernetes agent — the run is pinned there rather than claimed by a
  standard agent and quietly run with only `resources.limits` honoured. Use
  `resources.limits` if the standard agent should remain eligible. See the
  [migration
  guide](../../operator-manual/migrations/podtemplate-subfield-routing.md).
- **Env `value` must be a string; a non-literal `env` requires Kubernetes.**
  A container `env` entry's `value` must be a YAML string. An unquoted
  number or boolean (e.g. `value: 8080`) is a **hard error at job start on
  both backends** — quote it (`value: "8080"`). An env entry with no
  `value` key at all (i.e. a `valueFrom`-style entry) now requires a
  Kubernetes agent — the run is pinned there rather than having the entry
  silently dropped on the standard agent. See the [migration
  guide](../../operator-manual/migrations/podtemplate-subfield-routing.md).
- **Every container needs a `name`.** A `podTemplate` container with no
  `name` is a **hard error at job start on both backends**
  (`podTemplate container at index N has no name`) — add a `name` to every
  entry in `spec.containers`.
- **Container and volume names must be valid DNS-1123 labels.** Every
  `podTemplate.spec.containers[].name` and `podTemplate.spec.volumes[].name`
  must match `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` and be 63 characters or fewer
  — lowercase alphanumerics and `-`, starting and ending alphanumeric (the
  shape Kubernetes itself requires for a container/volume name). This is
  checked at **apply time** for both a `Job`'s inline `podTemplate` and a
  `JobTemplate`'s `podTemplate` — an invalid name (uppercase, underscores,
  leading/trailing `-`, embedded whitespace, `.`, too long, or empty) fails
  `apply`/run-creation immediately, naming the offending value, instead of
  surfacing later as an opaque pod-build API error. Example:
  `podTemplate container name "My_Tools" is not a valid DNS-1123 label
  (lowercase alphanumerics and '-', must start/end alphanumeric)`. This also
  closes a case/whitespace evasion of the reserved-name checks below
  (`job`/`unified-artifact`/`ucd-shim`/`workspace`/`ucd-tools`): those
  comparisons are normalized (trimmed, lowercased), but shape validation now
  rejects a variant like `" Job "` outright before it can reach that check.
- **A step-targeted container must not set `workingDir`.** Steps always run
  at the workspace mount: artifact/cache path resolution and
  `UNIFIED_WORKSPACE` both resolve there, and the artifact sidecar can only
  reach files under the workspace volume. A `workingDir` set on a container
  that steps execute in — the primary `job` container, or any container
  named by a step's (or parallel sub-step's, or `finally:` step's)
  `container:` field — would silently desync the shell's cwd from where
  artifacts/cache/`UNIFIED_WORKSPACE` actually resolve, so it is a **hard
  error at apply time** (checked on both a `Job`'s inline `podTemplate` and
  its `podTemplate.override.containers`), naming the offending container:
  `podTemplate container "NAME" declares workingDir, but steps execute in
  it: steps always run at the workspace mount (artifact/cache paths and
  UNIFIED_WORKSPACE resolve there); move the cd into the step script, or put
  workingDir on a sidecar`. Containers no step targets (true sidecars) may
  set `workingDir` freely — put the `cd` inside the step's `run:`/`shell:`
  script instead of relying on the container's `workingDir`.

**Routing is automatic and capability-based**, not selector-based: the
controller infers whether a `podTemplate` needs real Kubernetes (a named
agent-side template, an `override` patch, a pod-spec field beyond
`containers`, or a container field the host claim pod can't honor) or is
host-runnable (plain `name`/`image`/`env`/`resources.limits` containers,
`workspace.pvc` — which degrades to a host bind mount). A host-runnable
`podTemplate` can run on **either** a standard agent or a k8s-agent with no
hand-written selector required to make that work; a Kubernetes-only
`podTemplate` is routed to a k8s-agent only. See [Capabilities and
routing](../../operator-manual/agents.md#capabilities-and-routing) for the full model.

See the [Kubernetes Integration Guide](../../operator-manual/kubernetes-integration.md) for full details.

The example below uses a named agent-side template and an `override` patch,
both of which always force Kubernetes regardless of `agentSelector` — so its
`agentSelector: [kind:kubernetes]` is redundant here, but harmless, and documents
the intent:

```yaml
spec:
  agentSelector:
    - kind:kubernetes
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
container](#container-targeting-a-podtemplate-container)).

```yaml
steps:
  - name: build
    run: go build ./...        # runs in first container (default)

  - name: scan
    container: trivy           # runs in the "trivy" container
    run: trivy rootfs /workspace/app
```

---

