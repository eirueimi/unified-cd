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
(`--runner-image`, default `ghcr.io/eirueimi/unified-cd-runner` at the tag of the agent's own release version, e.g. `:v0.7.0`) as
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
  fleet. An agent whose record carries no capabilities at all (a
  pre-upgrade binary, or a record a heartbeat re-created before the agent's
  next claim poll restored them) is treated as a host agent that advertises
  `native` and `container`; if such an agent is really a k8s-agent and claims
  a native job by label match alone, it fails the run immediately with a clear
  error as a safety net.
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
PodSpec fields (`volumeMounts` and any other container field outside the
host-supported set) already require a Kubernetes agent — the run is pinned
there rather than degraded on the standard agent. `securityContext` is
honoured in part: see [Running as a non-root
user](#running-as-a-non-root-user) for which fields run on a standard agent
and which still pin the job to Kubernetes. An `env`
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
- **`resources.limits` keeps a container host-eligible only for `cpu` and
  `memory`, spelled as YAML strings.** The standard agent's `resources.limits`
  handling reads exactly those two keys as strings and silently drops
  everything else. An extended resource (`nvidia.com/gpu`,
  `ephemeral-storage`, `hugepages-*`, ...), `resources.claims`, or a bare
  numeric value (e.g. `cpu: 1` — valid Kubernetes `Quantity` syntax the
  host's string check just drops) now requires a Kubernetes agent, the same
  as `resources.requests`. See the [migration
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
`containers` and `securityContext`, a container field the host claim pod can't
honor at all, a `securityContext` value the host doesn't honor (see [Running as
a non-root user](#running-as-a-non-root-user)), or a sub-field of a
host-supported field the host only partially honors — `resources.requests`; a
`resources.limits` key other than `cpu`/`memory` as a string, including
`resources.claims`; or an `env` entry without a literal `value`) or is
host-runnable (plain `name`/`image` containers, literal `env` values,
`resources.limits` restricted to `cpu`/`memory` as strings, a
`workspace.mountPath`, a host-honoured `securityContext`). A host-runnable
`podTemplate` can run on **either** a standard agent or a k8s-agent with no
hand-written selector required to make that work; a Kubernetes-only
`podTemplate` is routed to a k8s-agent only. This is judged on the spec that
will actually run, so a `uses:` git template that contributes a Kubernetes-only
field (such as pod-level `volumes`) moves the run to a k8s-agent once it is
resolved. See [Capabilities and
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
      mountPath: /workspace   # an emptyDir: empty at the start of every run
    spec:
      containers:
        - name: job
          image: golang:1.24-alpine

    reuse: false              # keep the pod alive after the run for the next run (the workspace is still emptied)
    cleanWorkspace: false     # wipe a volume mounted at "." before the run (an ordinary workspace is always emptied)
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

### Running as a non-root user

A `securityContext` in the `podTemplate` runs the job's containers as a chosen user and group, on
a Kubernetes agent and on a standard agent alike. The standard agent honours these fields:

| Level | Fields |
|---|---|
| Pod (`podTemplate.spec.securityContext`) | `runAsUser`, `runAsGroup`, `runAsNonRoot`, `supplementalGroups`, `fsGroup`, `fsGroupChangePolicy` (`Always` or `OnRootMismatch`), `supplementalGroupsPolicy: Merge` |
| Pod, hardening | `seccompProfile` and `appArmorProfile` with `type: RuntimeDefault` (the runtime's default profile, which docker and podman already apply where the host supports it) |
| Container (`containers[].securityContext`) | `runAsUser`, `runAsGroup`, `runAsNonRoot` (a container value overrides the pod value, as in Kubernetes) |
| Container, hardening | `allowPrivilegeEscalation: false` (`no-new-privileges`), `capabilities.drop` (any capability, `ALL` included) and `capabilities.add` limited to `NET_BIND_SERVICE`, `readOnlyRootFilesystem: true`, `privileged: false`, `procMount: Default`, `seccompProfile` / `appArmorProfile` with `type: RuntimeDefault` |

A job that sets only these fields is routed to any Kubernetes agent, or to a standard agent that
advertises the `securitycontext` capability (see [Capabilities and
routing](../../operator-manual/agents.md#capabilities-and-routing)). A standard agent earns that
capability with a startup probe; one whose container runtime cannot deliver the semantics never
claims such a job, rather than running it with the fields silently dropped.

**These still pin the job to a Kubernetes agent**, because the standard agent cannot honour them:
`sysctls`, `seLinuxOptions`, `windowsOptions`, `supplementalGroupsPolicy: Strict`,
`seccompProfile` and `appArmorProfile` of type `Unconfined` or `Localhost`, `privileged: true`,
`capabilities.add` of anything but `NET_BIND_SERVICE`, a `procMount` other than `Default`, and any
key not listed above. A `securityContext` that mixes honoured and pinned values is pinned as a
whole.

With `readOnlyRootFilesystem: true` the container's root filesystem, `/tmp` included, is read-only
on both kinds of agent (a standard agent does not add the writable `/tmp` that podman would
otherwise mount). The workspace and [named volumes](volumes.md) are mounts, so they stay writable:
write temporary files under `$UNIFIED_WORKSPACE`. `capabilities.drop: [ALL]` leaves even root with
no effective capabilities, and `allowPrivilegeEscalation: false` sets `NoNewPrivs` for the
container. These apply to the container that sets them only, as in Kubernetes.

```yaml
spec:
  podTemplate:
    spec:
      securityContext:        # pod level: applies to every container and to files the platform writes
        runAsUser: 1000
        runAsGroup: 1000
        fsGroup: 1000
        fsGroupChangePolicy: OnRootMismatch
      containers:
        - name: job
          image: my-builder:latest
```

**Set `runAsUser` and `fsGroup` at the pod level.** Files the platform writes into the workspace for
the job (an artifact download, a cache restore) are owned by the *pod-level* `runAsUser` /
`runAsGroup`, or by `root:root` when only a container sets `runAsUser`. A job that sets the
identity only on its container can then not modify the files `download-artifact` fetched. That is
what Kubernetes does too, and the standard agent reproduces it.

**`fsGroup` and large volumes.** `fsGroup` makes the workspace and every [named volume](volumes.md)
group-writable for that group. Under the default `fsGroupChangePolicy: Always` the group and mode
of the whole volume are rewritten on every run, as on Kubernetes. For a volume of tens of GB set
`fsGroupChangePolicy: OnRootMismatch`: the walk is skipped when the volume root already has the
right group.

What you can observe, on either kind of agent:

| In a step | Result |
|---|---|
| `id -u`, `id -g`, `id -G` | the configured user, group and supplementary groups (`fsGroup` and `supplementalGroups` included) |
| `stat -c '%a %g' "$UNIFIED_WORKSPACE"`, no `fsGroup` | mode `777`; the group may differ between agents, writability does not |
| the same, with `fsGroup: F` | `2777 F` |
| a named volume root, with `fsGroup: F` | `2775 F` on a standard agent; on Kubernetes it depends on the storage driver (below) |
| owner of a file a step creates | the step's user, and group `F` through the setgid directory (otherwise the step's group) |
| owner of a downloaded artifact | pod-level user and group (or `root:root`), as above |
| `runAsUser` above the highest ID of a rootless runtime (typically 65536) | on a standard agent, the run fails at claim-pod start with an explicit message; on Kubernetes it runs |

`runAsNonRoot: true` makes the run fail before any container starts when the effective user would
be root (`container has runAsNonRoot and image will run as root`), as Kubernetes does.

Two differences remain on Kubernetes. Whether a PersistentVolumeClaim is regrouped by `fsGroup` at
all depends on the storage driver's `fsGroupPolicy` (`File` always, `ReadWriteOnceWithFSType` only
with a filesystem type, `None` never); `hostPath` and the `local-path` provisioner never regroup. And
a pod-level `runAsNonRoot` without a pod-level `runAsUser` can block the platform's own root-image
helper containers. See [Permission denied writing to a volume as a non-root
user](../../troubleshooting/steps-and-execution.md#permission-denied-writing-to-a-volume-as-a-non-root-user).

### podTemplate fields

| Field | Type | Description |
|---|---|---|
| `name` | string | Name of a template defined in the k8s-agent config file |
| `spec` | map | Inline Kubernetes PodSpec (used when `name` is empty) |
| `workspace.mountPath` | string | Path inside the pod where the (ephemeral) workspace is mounted |
| `reuse` | bool | Return the pod to a pool after the run and reuse it for subsequent runs. This only saves Pod start-up: the workspace of a reused pod is emptied before every run, and the run fails (the pod is deleted) if that is impossible. Not allowed together with `spec.volumes` |
| `cleanWorkspace` | bool | Empty a [volume](volumes.md) mounted at the workspace root (`mountPath: "."`) before the run. Equivalent to `spec.cleanWorkspace`; the two are ORed. An ordinary workspace is always emptied, so this changes nothing without such a volume |
| `override.containers` | []map | Additional containers to merge into the pod spec |
| `override.volumes` | []map | Additional volumes to merge into the pod spec |

The workspace is **always ephemeral**. The `pvc` block that an earlier release accepted under `workspace`
(a persistent claim or a per-Pod ephemeral one) has been removed, and a job that
still carries it is rejected at apply time; persistence now comes from a [named
volume](volumes.md). See the [migration
guide](../../operator-manual/migrations/2026-10-20-named-volumes.md).

> **`workspace.mountPath` alone is honored on every agent.** A `podTemplate` that sets *only*
> `workspace.mountPath`, with neither `name` nor `spec`, mounts the workspace at that path on a
> Kubernetes agent (in its default Pod) just as on the standard agent.

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

