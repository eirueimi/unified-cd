# Kubernetes Integration Guide

unified-cd can integrate with Kubernetes clusters through the `k8s-agent`. For each job it receives, the agent spawns a Pod to execute the steps and deletes the Pod when finished.

---

## Architecture

```
unified-cd-master
       │  HTTP (claim / report)
       ▼
  k8s-agent  ─────────────────────────────────────────────────────
  (runs inside or outside the cluster)                            │
       │                                                          │
       │ Kubernetes API                                           │
       ▼                                                          │
  Job Pod (namespace: ci)                                         │
  ┌─────────────────────────┐                                     │
  │  container: job         │← steps executed via exec            │
  │  container: sidecar … │← switching to another container ok  │
  │  volume: /workspace     │← emptyDir (+ PVCs for named volumes)│
  └─────────────────────────┘                                     │
                                                                  │
  PodPool (when reuse: true)                                      │
  ┌──────────────────────────┐                                    │
  │ existing Pods pooled for reuse │─────────────────────────────┘
  └──────────────────────────┘
```

The k8s agent implements the same step DSL and master-communication interface as the standard
agent (`cmd/unified-cd-agent`); job steps run inside a Pod instead of locally. Orchestration itself is now a
single shared implementation (`internal/agent`'s `RunClaim`, driven through the `ExecBackend`
seam) — only the execution backend differs per agent. The remaining intentional differences are:

- **`container:`** — supported on both agents as the canonical way to target a named
  `podTemplate` container. On k8s it execs into the named container of the job Pod. On
  the standard agent it execs into the corresponding container of the claim pod (see
  [Job Isolation: `native` and the claim
  pod](../user-guide/writing-jobs/isolation-and-containers.md#job-isolation-native-and-the-claim-pod)); a sidecar's `command`/`args`
  now match standard Kubernetes/OCI ENTRYPOINT/CMD semantics on **both** backends —
  see [Host container command/args
  semantics](#host-container-commandargs-semantics) below for the full truth table
  and per-runtime support matrix. Other host-unsupported `podTemplate` fields (a named
  agent-side template, an override patch, extra pod-spec beyond `containers`,
  `volumeMounts`, and any other container field outside the host-supported set)
  already require a Kubernetes agent — the run is pinned there, not degraded on the
  standard agent. An
  `env` entry that sets `valueFrom` instead of a literal `value` also requires a
  Kubernetes agent: the standard agent's docker/podman backend has no Kubernetes API
  to resolve `valueFrom` against, so a container that needs it is pinned to a
  Kubernetes agent rather than having the value silently dropped. See the [migration
  guide](migrations/podtemplate-subfield-routing.md) if this changes which runs
  schedule for you. Unlike k8s, the standard agent's claim-pod containers
  share one network namespace (via the pause container), so — unlike the old MVP
  single-container form this replaces — sidecars **are** reachable at `localhost` from
  every claim-pod container, matching k8s.
- **Resource `requests`** (`podTemplate.spec.containers[].resources.requests`) — applied
  only on Kubernetes, because docker/podman/nerdctl have no request concept for the
  standard agent to map it onto. A `podTemplate` container that sets
  `resources.requests` now requires a Kubernetes agent: the run is pinned there rather
  than claimed by a standard agent and quietly run with only `resources.limits`
  honoured. Use `resources.limits` if the standard agent should remain eligible, or
  see the [migration guide](migrations/podtemplate-subfield-routing.md). **This only
  applies to `cpu` and `memory` spelled as YAML strings.** The standard agent's
  `resources.limits` handling reads exactly those two keys as strings and silently
  drops everything else — an extended resource (`nvidia.com/gpu`,
  `ephemeral-storage`, `hugepages-*`, ...), `resources.claims`, or a bare numeric
  `cpu`/`memory` value (e.g. `cpu: 1`, valid Kubernetes `Quantity` syntax the host's
  string check just drops). Any of those now requires a Kubernetes agent too, the
  same as `resources.requests`. Separately, a Kubernetes agent with `workspaceSizeLimit` set
  injects an `ephemeral-storage` **request** into the `job` container itself; it does so only
  when nothing in the template already sets `ephemeral-storage`, and it never overrides yours (see
  [Disk limits and headroom](#disk-limits-and-headroom)).
- **`native: true`** — host-only. A `native: true` job claimed by the k8s-agent fails the
  run immediately with a clear error; route native jobs away from k8s-agents (and to host
  agents) via `agentSelector`.
- **Drain window** — on shutdown (SIGTERM/rollout) the k8s agent stops claiming immediately
  but lets in-flight runs keep going, same as the standard agent's `--drain-timeout`; see
  [Resilience & concurrency](#resilience-concurrency) below. Any run still in flight when the
  Pod is actually killed (drain window elapsed, or the process was force-killed) is recovered
  by the startup reconcile / stuck-run reaper on the next agent start.

Feature parity between the two agents is enforced by the shared conformance suite
(`internal/paritycases`) — new DSL behavior must pass identical expectations on both agents.

### Host container command/args semantics

A `podTemplate` container's `command:`/`args:` mean the same thing on both
backends now — standard Kubernetes/OCI ENTRYPOINT/CMD override semantics:

| `command` | `args` | Resulting process (both backends) |
|---|---|---|
| unset | unset | The image's own `ENTRYPOINT` + `CMD`, unmodified (e.g. a sidecar's own service command, `mysqld`). |
| unset | set | The image's own `ENTRYPOINT`, invoked with `args` as its arguments (image `CMD` replaced). |
| set | unset or set | `command` replaces the image `ENTRYPOINT`; `args` (if also set) follow as its arguments. The image `ENTRYPOINT` is never invoked. |

On k8s this was already native `corev1.Container` behavior and is
unchanged. On the standard agent, this is a **breaking change** from the
previous behavior, where `command` and `args` were merged into one
positional `CMD` override and the image's `ENTRYPOINT` always ran
regardless of `command`; a job that relied on that merge behavior should set
`command`/`args` explicitly to match the per-field semantics described above.

**On both backends, the primary `job` container's own image `ENTRYPOINT`
is always ignored**, regardless of any `command`/`args` a `podTemplate` sets
on it — the pod build unconditionally forces it to the `ucd-sh pause`
keep-alive (via an `ENTRYPOINT` override on the standard agent, via a
`Command` override on k8s), so it stays alive as the exec target for
`container:`-less steps. This applies uniformly to the primary `job`
container on **both** the standard agent's claim pod and the k8s-agent's
job Pod — see [Keep-alive: `ucd-sh pause`](#keep-alive-ucd-sh-pause) below.
Sidecar containers on both backends still honor `command`/`args` as
described in the table above; only the primary `job` container's
`command`/`args` are discarded.

#### Per-runtime support for the ENTRYPOINT clear (standard agent only)

On the standard agent, replacing a container's `ENTRYPOINT` (the `command`
column above) requires the container CLI to support the empty-clear form
(docker's `--entrypoint ""`, emitted before the image). Support is recorded
per runtime, verified on real binaries — not assumed:

| Runtime | `--entrypoint ""` empty-clear | Status |
|---|---|---|
| docker | Supported | Verified (Docker 29.6.1) |
| podman | Unverified | Not present on the verification machine; not tested |
| nerdctl | Unverified | Not present on the verification machine; not tested |
| wslc | Unverified | Not present on the verification machine; not tested |
| Apple `container` | Unverified | Not available on the verification machine (Windows); not tested |

A runtime confirmed **not** to support the empty clear is added to
`internal/runtime`'s `noEmptyEntrypointClear` set (currently empty — no
runtime has failed verification). For a runtime in that set, a `command`
override degrades to the pre-parity behavior: `command`+`args` run as
positional `CMD` and the image's own `ENTRYPOINT` still executes, plus one
`WARN` log naming the runtime and the limitation. This never silently
produces a broken command — it produces a diagnosed fallback to the old,
still-functional-if-imprecise behavior.

---

## Setup

### 1. Config file

Create `k8s-agent-config.yaml`:

```yaml
# HTTPS endpoint of an externally provided TLS terminator and workload enrollment policy
server: https://unified-cd-master.example.com
enrollmentPolicy: unified-cd-k8s-agents
serviceAccountTokenFile: /var/run/secrets/unified-cd-agent/token
labels:
  - kind:kubernetes   # requested label; controller policy is authoritative

namespace: ci          # Kubernetes namespace where job Pods are created
maxConcurrent: 100     # max concurrent Pods (0/unset -> 100; negative -> unlimited; see below)

# Fallback image when no podTemplate is specified. Bash-less images (as here)
# work fine by default — steps exec via the injected ucd-sh shim, not bash.
podImage: golang:1.24-alpine

# Image the prepended init container runs to install the ucd-sh shim onto
# the shared /.ucd volume (see "Step execution mechanism" below). Defaults
# to the k8s-agent's own image at the tag matching the agent's version
# (:latest for a locally built agent); the image ships /ucd-sh at its root.
# Set a digest to pin it (see "Shim image: pin it in production" below).
# shimImage: ghcr.io/eirueimi/unified-cd-k8s-agent:v0.7.0

# kubeconfig omitted → uses InClusterConfig if running inside the cluster,
#                       or ~/.kube/config if running outside
# kubeconfig: /path/to/kubeconfig

# Agent-owned named volumes jobs mount with spec.volumes (see "Named volumes" below).
# agentName is required when volumes is set and must never change while they hold data.
# agentName: ci-main
# volumes:
#   - name: go-cache
#     mode: exclusive
#     size: 20Gi

# Templates registered with this agent (referenced by name in Job YAML)
podTemplates:
  golang:
    workspace:
      mountPath: /workspace
    spec:
      containers:
        - name: job
          image: golang:1.24-alpine
          # command omitted → agent auto-injects ["/.ucd/ucd-sh", "pause"]

  node:
    workspace:
      mountPath: /workspace
    spec:
      containers:
        - name: job
          image: node:20-alpine
```

Agent templates are operator-written and are **not** subject to the [Job Pod field policy](#job-pod-field-policy); a template may set any Pod field. The one limit is the namespace's Pod Security level: the shipped `ci` namespace enforces `baseline`, which refuses `privileged: true`, `hostPath` and the like in a template as well. If you add a template with a Docker-in-Docker container (`privileged: true`), relabel the namespace or run it from a separate agent and namespace.

Job-side policy opt-ins go in the same file, in a `jobPodPolicy:` block (see [Job Pod field policy](#job-pod-field-policy)). Leave it out for the most restrictive policy.

### 2. Starting the agent

Before starting a Pod agent, configure the controller with a Kubernetes cluster verifier and an enabled policy that binds the exact agent ServiceAccount and namespace. The default manifests declare `in-cluster` and `unified-cd-k8s-agents`; its policy accepts only ServiceAccount `unified-cd-k8s-agent` in namespace `unified-cd`, `kind:kubernetes`, and `pod`/`container` capabilities. The controller must run with the TokenReview and bounded Pod-read RBAC in `manifests/base/controller/rbac.yaml`.

The agent Pod mounts a projected ServiceAccount token with audience `unified-cd-agent-enrollment`. It exchanges that token for a short-lived access token in memory; it never stores a refresh token or receives a shared controller token.

The k8s-agent has no `make build` target; build it from source or use its Docker image:

```bash
go build -o bin/unified-cd-k8s-agent ./cmd/k8s-agent
```

```bash
# Inside the cluster (running as a Pod, no kubeconfig needed)
./bin/unified-cd-k8s-agent --config k8s-agent-config.yaml

# Via environment variable
UNIFIED_K8S_CONFIG=k8s-agent-config.yaml ./bin/unified-cd-k8s-agent
```

The install bundles (`install.yaml`, `core-install.yaml`, `agent-only.yaml` — built from the `manifests/*` kustomize overlays and published as release assets) default the `unified-cd-k8s-agent` Deployment to `replicas: 2`, running active-active; see [Agent Redundancy](high-availability.md#agent-redundancy) in the HA guide for why this is safe.

---

## Resilience & concurrency

Four config fields (full reference: [Configuration: K8s Agent config
fields](../reference/configuration.md#k8s-agent-config-fields)) bound how the k8s-agent behaves under
scheduling pressure, during cleanup, and during shutdown:

| Field (yaml) | Env override | Default | Behavior |
|---|---|---|---|
| `podStartTimeout` | `UNIFIED_K8S_POD_START_TIMEOUT` | `5m` | Bounds how long the agent waits for a run Pod to reach `Running` before failing the run. Without this, an unschedulable or `ImagePullBackOff` Pod would wedge the run forever — under `RestartPolicy: Never` a stuck-Pending Pod never transitions to `Failed` on its own. The wait also aborts early (without overriding the controller's status) if the run is already terminal at the controller. |
| `drainTimeout` | `UNIFIED_K8S_DRAIN_TIMEOUT` | `0` (wait indefinitely) | On SIGTERM/rollout, the agent stops claiming new runs immediately but lets in-flight runs keep going — heartbeats keep beating throughout drain so a draining run isn't reaped as stuck — until they finish or `drainTimeout` elapses, whichever is first. `0`/unset never forces a cutoff. Parity with the standard agent's `--drain-timeout`. |
| `finallyTimeout` | `UNIFIED_K8S_FINALLY_TIMEOUT` | `10m` | Bounds **each** post-DAG cleanup phase of a run: each `post:`/`cache:` hook drain, the [`spec.finally`](../user-guide/writing-jobs/approval-and-finally.md#finally-block-finally) pipeline, and scope/Pod teardown — plus a **fifth** window on this agent, for the claim Pod's own deletion or return to the idle pool, which runs after the shared cleanup loop returns and so cannot share the fourth. **Size a rollout's `terminationGracePeriodSeconds` against 5 × this value (50m at the default)**; four windows, 40m, is the standard agent's number, which tears its claim pod down inside the fourth. Every one of those windows is enforced against the Kubernetes API server directly; no `rest.Config.Timeout` is set, and none can be, because the same client config drives exec streams and follow-mode log reads, which are legitimately long-lived. Those phases deliberately survive run cancellation, which also discards the job-level `spec.timeoutMinutes` deadline — so without this ceiling a cleanup step that never returns (most easily a `call:` waiting on a child run that will never be claimed) pins the run and its concurrency slot indefinitely, and nothing trips: the stuck-run reaper keys on agent liveness, and the agent keeps heartbeating. Non-positive falls back to the default; "unbounded" is not expressible; an unparseable value is rejected at startup and the agent exits 1. A phase cut short by this budget is recorded on the run's own **System** stream — for a hook drain that record is the only signal, since a post hook never changes a run's status. Parity with the standard agent's `--finally-timeout`. |
| `maxConcurrent` | — | `100` | Max simultaneous job Pods, enforced by a semaphore around the claim loop. `0`/unset → `100` (raised from the previous default of `5`). A **negative** value (e.g. `-1`) removes the agent-side cap entirely — concurrency is then bounded only by cluster scheduling/quota. A positive value is an exact concurrency bound. |

Env vars, where present, override the config-file value (see [Configuration: Priority
Order](../reference/configuration.md#priority-order) — CLI flags still win over both, but these fields have
no CLI flag).

**API-server load.** The agent sets its Kubernetes client's rate limit itself, from `maxConcurrent`:
`2 × maxConcurrent + maxConcurrent/5 + 20` queries per second, with the same figure as the burst
(240 at the default of 100; the same figure applies when `maxConcurrent` is negative, sized as the
default). It is not configurable, and it replaces any QPS or burst a kubeconfig carried. The default
client limit of 5 QPS is shared by every call and would make a Pod create wait minutes behind the
agent's own start-up polling (about 2 QPS per starting run). Step `exec` streams are not counted
against it. The practical consequence is that a fleet of this agent can send up to that rate to the
API server, so size `maxConcurrent` against your API server's capacity.

---

## podTemplate in Job YAML

### Pattern 1: Named template reference

Reference a template defined in the agent config file by name.

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: go-build
spec:
  agentSelector:
    - kind:kubernetes
  podTemplate:
    name: golang        # uses podTemplates.golang from k8s-agent-config.yaml
  steps:
    - name: build
      run: go build ./...
    - name: test
      run: go test ./...
```

### Pattern 2: Inline PodSpec

Specify the PodSpec directly in the Job without a pre-defined template.

A Kubernetes agent refuses some inline fields (a ServiceAccount, `hostPath`, `privileged`, Secrets, PVCs and more) unless its operator allows them; see [Job Pod field policy](#job-pod-field-policy).

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: python-lint
spec:
  agentSelector:
    - kind:kubernetes
  podTemplate:
    workspace:
      mountPath: /workspace   # an emptyDir: empty at the start of every run, gone with the Pod
    spec:
      containers:
        - name: job
          image: python:3.12-slim
  steps:
    - name: lint
      run: ruff check /workspace
```

### Pattern 3: Multi-container

Add containers to the template and switch the execution container per step.

```yaml
spec:
  podTemplate:
    name: golang
    override:
      containers:
        - name: trivy
          image: aquasec/trivy:latest   # agent auto-injects ["/.ucd/ucd-sh", "pause"]
  steps:
    - name: build
      run: go build -o /workspace/app ./cmd/server
      # container omitted → runs in the "job" container

    - name: scan
      container: trivy                  # /workspace is shared across all containers
      run: trivy rootfs /workspace/app --exit-code 1
```

### Pattern 4: Pod reuse (faster start)

With `reuse: true`, the Pod is returned to a pool after the run and reused by the next run, so that
run skips scheduling and image pulls. **It does not carry the workspace over**: the agent empties
the workspace of a reused Pod, dot-files included, before every run. If it cannot, the run fails
(`k8s: could not clear the reused Pod's workspace: ...`) and the Pod is deleted rather than returned
to the pool. Earlier releases kept `/workspace` between runs of a reused Pod, and
`podTemplate.cleanWorkspace: false` was how you asked for that; it no longer has any effect on an
ordinary workspace.

```yaml
spec:
  podTemplate:
    name: golang
    reuse: true
  steps:
    - name: restore-modules
      cache:
        path: .gomodcache
        key: go-mod-{{ hashFile "go.sum" }}
    - name: build
      run: GOMODCACHE="$UNIFIED_WORKSPACE/.gomodcache" go build ./...
```

For data that must survive between runs use `cache:` (cheap to move) or a [named
volume](#named-volumes) (expensive to move). A job that mounts a volume cannot also set
`reuse: true`; apply rejects the combination.

---

## Workspace (`/workspace`) behavior

| Configuration | Behavior |
|---------------|----------|
| `workspace` not set, or only `mountPath` set | `emptyDir` (empty when the run starts, deleted when the Pod is deleted) |
| `spec.volumes` names a volume at `mountPath: "."` | That volume's PVC *is* the workspace and persists between runs |
| `spec.volumes` names a volume at any other path | The workspace stays an `emptyDir`; the PVC appears at that path |

All containers in the Pod mount the same path (`mountPath`), so files are shared between containers.

The `emptyDir` is unlimited unless the agent's `workspaceSizeLimit` (or the template's
`podTemplates.<name>.workspaceSizeLimit`) is set, in which case it gets a `sizeLimit` and the `job`
container an `ephemeral-storage` request; see [Disk limits and headroom](#disk-limits-and-headroom).

The workspace is **always ephemeral** unless a named volume replaces it, on this agent and on the
standard agent alike. The `pvc` block that an earlier release accepted under `workspace` (a persistent claim or a
per-Pod ephemeral one) has been removed; a job or agent config that still carries it is rejected. See the [migration
guide](migrations/2026-10-20-named-volumes.md).

> **A `podTemplate.workspace.mountPath` on its own is honored.** A job whose `podTemplate` sets *only*
> `workspace.mountPath`, with neither `name` nor `spec`, gets the agent's default Pod with the
> workspace mounted at that path, exactly as on the standard agent. The mount path is also where
> `UNIFIED_WORKSPACE`, relative artifact and cache paths, volume mounts and the workspace wipe resolve.

---

## Named volumes

A k8s-agent's Pod workspace is an `emptyDir`: it is empty when the run starts and dies with the
Pod. The only thing that survives between runs is a **named volume**, which the agent owns as a
PVC and a job mounts by name with `spec.volumes`. The job-author view is [Volumes: Keeping Data
Between Runs](../user-guide/writing-jobs/volumes.md); the host-agent equivalent is [Named
volumes](agents.md#named-volumes). This section is the Kubernetes specifics.

```yaml
# k8s-agent-config.yaml
agentName: ci-main            # required when `volumes` is set
volumes:
  - name: unity-main          # a DNS label
    mode: exclusive           # ReadWriteOnce
    size: 200Gi               # required on Kubernetes
    storageClassName: gp3     # optional; unset means the cluster default
  - name: go-cache
    mode: shared              # ReadWriteMany: needs an RWX StorageClass
    size: 50Gi
```

**`agentName`.** The PVCs are named `<agentName>-<volume>` (`ci-main-unity-main`). It is a separate
field, not the agent ID, because the enrolled ID is `k8s:<cluster>:<namespace>:<pod UID>`: it
contains colons a claim name cannot, and it changes on every restart, so a prefix derived from it
would provision a fresh, empty PVC on each restart and leave the last one behind. Replicas of one
Deployment share a config and so share one set of PVCs. Two k8s-agents in one namespace need two
different `agentName`s. **Never change `agentName` while the volumes hold data**: the new name
provisions new, empty PVCs and leaves the old ones behind, still holding your data and still
costing money.

**Volume names must be DNS labels.** Lower-case letters, digits and `-`, and `<agentName>-<volume>`
must be at most 63 characters. A name that is legal on a host agent (which accepts almost any
single path segment) can therefore be illegal here, and the agent refuses to start with an error
naming the volume. A job that mounts such a volume on both agent types should use lower-case
DNS-label names throughout.

**PVC lifecycle.**

- The agent creates a missing PVC at startup with the configured `size`, the access mode implied
  by `mode`, and the `storageClassName` **only if you set one**; left unset, the PVC requests the
  cluster's default StorageClass rather than the empty class that means "no StorageClass".
- It **never resizes or deletes** one. If an existing PVC's size or access mode differs from the
  config it logs a warning and leaves the PVC alone; expand it through your StorageClass, or delete
  and recreate it yourself.
- A volume dropped from the config stops being advertised and its PVC stays, logged as an orphan.
  Removing a line from a config file must not destroy tens of gigabytes.
- **Free space.** The agent measures each volume's filesystem from inside the claim pod, before the
  run and again after it, and the controller exports it as
  `unifiedcd_agent_volume_filesystem_free_bytes{agent_name,volume}`. An idle volume is not measured,
  so its figure ages (see `unifiedcd_agent_volume_measured_timestamp_seconds`), and a restarted agent
  has no figures until a run next mounts each volume. Replicas sharing one config report the same PVC
  under different `agent` IDs: aggregate with `min by (agent_name, volume)`. See [Watching agent disk
  headroom](operations.md#watching-agent-disk-headroom).
- **Low free space holds runs back.** The same readings feed `minFreeDisk` (default `1%`): a volume
  below it is withheld from the claim and the runs that mount it stay `Queued`. It has a
  10-minute staleness escape and a restart clears it; see [Disk limits and
  headroom](#disk-limits-and-headroom).

**Exclusion across replicas.** `mode: exclusive` is not left to the `ReadWriteOnce` access mode,
which is a *node*-level constraint and says nothing about two Pods on one node. Instead the agent
takes a lock on the PVC itself: a run writes its ID to the PVC's `unified-cd/volume-held-by`
annotation before its Pod is created and clears it when the Pod has been deleted. Occupancy is read
from the live Pods that mount each PVC, so it is correct across replicas and across an agent
restart. A run that finds its volume held waits, holding its concurrency slot, with
`waiting for volumes [[...]], which another run is using` in its log.

> **A crashed holder blocks its exclusive volume for up to about three minutes.** A lock whose run
> has no live Pod is taken over only after the two-minute grace that protects a lock whose Pod is
> still being created, plus the Pod garbage-collection cadence. It clears itself. See [A volume
> stays held after its run
> crashed](../troubleshooting/steps-and-execution.md#a-volume-stays-held-after-its-run-crashed).

**`mode: shared`** requests `ReadWriteMany`, which binds only on a StorageClass that supports RWX.
In practice that means a network filesystem (NFS, CephFS, EFS, Azure Files, Longhorn), which is slow
for many-small-file workloads such as a Go module cache or a Unity `Library/`. Where RWX is not
available, or is too slow, declare several `exclusive` volumes instead and let jobs list them as
candidates (`name: [go-cache-1, go-cache-2, go-cache-3]` with `as:`): the agent picks a free one
and you get the same parallelism without RWX. A `shared` volume cannot be mounted at the workspace
root. If RWX is unavailable the PVC stays `Pending` and a run fails after `podStartTimeout`; see
[A shared volume never binds](../troubleshooting/steps-and-execution.md#a-shared-volume-never-binds-rwx-unavailable).

**A freshly provisioned PVC is usually root-owned**, so a container that runs as a non-root user
cannot write to it. Give the job's Pod a `securityContext.fsGroup` that matches the container's
group, in its `podTemplate`:

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

(`securityContext` is Kubernetes-only, so such a job is pinned to a Kubernetes agent.) `fsGroup` and `runAsUser` are allowed by the [Job Pod field policy](#job-pod-field-policy) with no opt-in.

**Mounting.** A volume is mounted at its `mountPath` in **every** container of the Pod, including
the `unified-artifact` sidecar, so artifact and cache paths inside a volume work and an archive of
an ancestor path excludes the volume (see [Artifacts and Cache](#artifacts-and-cache)). A scoped
(`runsIn.image`) step runs in its own container that mounts none. `podTemplate.reuse: true` cannot
be combined with `spec.volumes`: a pooled Pod keeps the volumes it was created with, and apply
rejects the combination.

**Creating the Pod is bounded.** For a volume-bearing run the agent gives each Pod create at most a
quarter of the two-minute lock grace (30 seconds). A create that times out, or that finds the
previous attempt's Pod still terminating, releases the run's locks and is retried; after **five**
consecutive failed attempts the run fails with `k8s: creating the run's pod did not complete in 5
consecutive attempts, with no wait for a busy volume between them; giving up. Last attempt: ...`. A
wait for a busy volume between two failures starts the count over, so a run that queued for hours
behind another is not failed for scattered, unrelated timeouts. Each retry is logged on the run as `creating the run's pod did
not complete (attempt N of 5: ...); its volume locks were released and it will try again`. A cluster
that is merely slow costs a run time; one that never answers does not hold the run and its slot
forever.

**Roll the Role before the image.** With `volumes` configured the agent reads, lists, creates and
updates PVCs at startup and when it takes a lock. The Role needs `get`, `list`, `create` and
`update` on `persistentvolumeclaims`; the Role shipped before named volumes had only `create` and
`delete`, so a new agent against an old Role fails at startup with a `forbidden` error. See [RBAC
example](#rbac-example). It never deletes a volume's PVC itself.

A run still waiting for its volumes when the agent starts draining is failed with `k8s: run
abandoned: the agent is draining and the run was still waiting for its volumes ...` instead of
holding up the drain.

---

## Disk limits and headroom

Two independent settings keep a Kubernetes agent from running out of disk. Neither is a quota: one
protects the cluster's nodes from a single run, the other protects your volumes from being filled
by runs that are admitted too late.

### Capping the workspace: `workspaceSizeLimit`

A run's workspace is an `emptyDir`, which lives on the node's disk and is not limited by default. A
run that writes without bound fills the node and the kubelet starts evicting unrelated pods. Set
`workspaceSizeLimit` (a Kubernetes quantity) to prevent that:

```yaml
# k8s-agent-config.yaml
workspaceSizeLimit: 20Gi          # global default; omit to leave the workspace uncapped (the default)

podTemplates:
  unity:
    workspaceSizeLimit: 80Gi      # overrides the global value for this template
    spec:
      containers:
        - name: job
          image: my-unity-builder:latest
```

(or `UNIFIED_K8S_WORKSPACE_SIZE_LIMIT` for the global value; the per-template key is
`podTemplates.<name>.workspaceSizeLimit`. The config loader ignores unknown keys, so check the
spelling: a misspelled key leaves the workspace uncapped without any error.)

When it applies, the agent sets `sizeLimit` on the `workspace` `emptyDir` **and** an
`ephemeral-storage` **request** of the same size on the `job` container. The request makes the
scheduler place the pod only on a node with that much ephemeral storage free; the `sizeLimit` makes
the kubelet evict the pod if the workspace grows past it. No `ephemeral-storage` *limit* is added,
because a container limit also counts logs and the container's writable layer and would kill runs
for reasons the operator did not choose. The limit is applied only when the workspace is an
`emptyDir` (not when a named volume replaces it) and when nothing in the pod template already sets
an `ephemeral-storage` request or limit, on any container or init container, or a `sizeLimit` on
the workspace: what you wrote in the template always wins. Scope pods (`runsIn.image` steps) get the
global value, and each one reserves the full global `workspaceSizeLimit` as its own
`ephemeral-storage` request, so a run with N `runsIn.image` scopes reserves (N+1) times the limit
across its pods. Pooled pods (`podTemplate.reuse`) are keyed by their resolved limit, so changing the
limit gives new runs new pods rather than reusing one created with the old cap.

**It is unset by default** because a limit that is too small kills legitimate runs, and the right
number depends on the largest job you run. While it is unset the agent logs at startup that "a run
can fill the node's ephemeral storage". Start from the largest workspace your jobs produce plus
margin, and watch for evictions (below).

When a run's pod is evicted, the run fails with the kubelet's own message first, then the hint:

```
k8s: pod <pod name> was evicted: Usage of EmptyDir volume "workspace" exceeds the limit "20Gi".
The workspace emptyDir is limited to 20Gi by the agent workspaceSizeLimit; use a larger podTemplate (podTemplates.<name>.workspaceSizeLimit), or keep large data in a named volume (spec.volumes).
(exec error: ...)
```

The hint appears only when a limit is configured and the kubelet message mentions `emptyDir` or ephemeral storage. See [Kubernetes run fails: pod was
evicted](../troubleshooting/artifacts-and-storage.md#kubernetes-run-fails-pod-was-evicted-usage-of-emptydir-volume-workspace-exceeds-the-limit).
To look for evictions on the cluster:

```bash
kubectl -n ci get pods --field-selector=status.phase=Failed
kubectl -n ci get events --field-selector reason=Evicted
```

### Gating claims on volume headroom: `minFreeDisk`

`minFreeDisk` (`1%` by default; also `"10Gi"`, a byte count, or `0` / `"off"`; env
`UNIFIED_K8S_MIN_FREE_DISK`) applies to the agent's [named volumes](#named-volumes). A volume whose
free space is below the threshold is withheld from the claim: runs that mount it stay `Queued`
(for another agent, or until space is freed) and other runs are unaffected. A volume that is not
being gated does not block anything, and there is no workspace gate on this agent, because the
workspace is an `emptyDir` governed by `workspaceSizeLimit` instead. A malformed value fails
startup. The Kubernetes agent's config loader ignores unknown keys, so spell the key exactly:
`minFreeDisk`.

A Kubernetes agent can measure a volume only from inside a pod that has the PVC mounted, so a
volume is measured only by a run that mounts it (before the run starts and again after it ends).
That shapes the gate:

- **A volume that was never measured is not gated.** A new or freshly restarted agent has no
  readings; the first run that mounts the volume goes through and measures it.
- **A reading older than 10 minutes no longer gates.** Nothing measures an idle volume, so a gated
  volume would otherwise stay gated forever even after you free space. Ten minutes after the last
  reading, **runs are let through until one of them measures the volume again**: there is no
  single probe, so a shared volume, or an agent running several runs at once, can start more than
  one in that window. A run's pre-run measurement either clears the gate or, if the volume is still
  low, re-arms it, and the run's own log then carries a warning such as
  `volume go-cache has 3.1GiB free (0.6%), below minFreeDisk 1%; this run may fail for lack of
  space`. The window is fixed; it is not configurable.
- **A restart clears the readings**, and with them the gate. Restarting the agent (`kubectl rollout
  restart deployment/unified-cd-k8s-agent`) is the way to lift a gate immediately after freeing
  space. The metric `unifiedcd_agent_volume_claim_blocked` reads `0` once a reading is more than 10
  minutes old, consistent with the claim decision.
- **Each replica keeps its own readings.** Replicas share one set of PVCs but not one cache, so one
  replica may be gating a volume while another, which measured it earlier or later, is not.
  Aggregate `volume_claim_blocked` with `max by (agent_name, volume)`.
- **A threshold that is not smaller than the volume** (an absolute `minFreeDisk` of `100Gi` on a
  `50Gi` PVC) can never be met, so that volume is not gated and the agent logs a WARN once.

The log lines are `volume below minFreeDisk, not claiming runs that mount it` when a volume enters
the gate and `volume no longer below minFreeDisk (or its reading is too old to trust), claiming
runs that mount it again` when it leaves. See [Run stays Queued: the volume it mounts is below
`minFreeDisk`](../troubleshooting/runs-and-scheduling.md#run-stays-queued-the-volume-it-mounts-is-below-minfreedisk)
and [Operations: Choosing a threshold](operations.md#choosing-a-threshold).

---

## Step execution mechanism

The k8s-agent follows these steps:

1. Create the Pod: prepend the `ucd-shim` init container (see below),
   unconditionally inject the `["/.ucd/ucd-sh", "pause"]` keep-alive into
   the primary `job` container, discarding any `command`/`args` a
   `podTemplate` set on it
2. Send each step into the Pod via the equivalent of `kubectl exec`, running
   `/.ucd/ucd-sh -c <script>` by default (or the step's effective `shell:`
   argv — see [Job Reference: Shell (`shell:`)](../user-guide/writing-jobs/steps.md#shell-shell))
3. Report results and logs to the master in real time
4. After all steps complete, delete the Pod (or return to pool if `reuse: true`)

Use `container:` to switch the execution container per step. When omitted, the first container (`job`) is used.

**`bash -lc` is gone as the hardcoded exec wrapper.** Every earlier version
of this agent exec'd steps with `bash -lc "<script>"`, which meant every
`podImage`/`podTemplate` container needed a working `bash` — a requirement
the DSL never stated and this doc's own `golang:1.24-alpine`/`alpine`
examples silently violated. Steps now exec via the injected `ucd-sh` shim by
default (`/.ucd/ucd-sh -c "<script>"`), which requires **no shell binary in
the image** — bash-less/sh-less images with coreutils (`alpine`,
busybox-based) are valid `job`/sidecar images. One remaining requirement on
this agent: exec-time environment variables are applied by prepending the
`env` binary, and every step carries at least `UNIFIED_AGENT_OS` — so a
truly empty image (`scratch`, distroless-static) can host the keep-alive
but fails env-carrying steps with exit 127. A job that relies on real bash
semantics (login-shell profile sourcing, `wait -n`, `PIPESTATUS`, signal
traps, ...) opts back in explicitly with `spec.shell: [bash, -lc]` or a
step-level override — see the [interpreter constraints
table](../user-guide/writing-jobs/steps.md#the-default-the-ucd-sh-shim) for exactly what the default
shim does and doesn't support.

### `/.ucd` shim injection

Every Pod this agent builds — job Pods, scope Pods (from a `uses:`-level
`runsIn.image`), and pool-reused Pods — gets:

- An `emptyDir` volume named `ucd-tools`, mounted at `/.ucd` on **every**
  container in the pod (the primary `job` container and every
  `podTemplate`/`override` sidecar — a sidecar is itself a `container:` exec
  target and needs the shim too).
- A **prepended** init container, `ucd-shim`, running the agent's
  `shimImage` (config field `shimImage`, default the k8s-agent's own image,
  which ships `/ucd-sh` at its root) with `command: ["/ucd-sh", "--install",
  "/.ucd/ucd-sh"]` — it self-copies onto the shared volume before any other
  container starts. This is the Tekton/Argo emissary init-container pattern:
  a Pod has no host filesystem to bind-mount the shim from, unlike the
  standard agent's claim pod (which bind-mounts its own tools directory
  read-only).
- If the `podTemplate`/`override` already declares `initContainers:`,
  `ucd-shim` is **prepended** ahead of them — the shim must be on disk
  before any user init container (or any regular container) that might also
  need it.
- `shimImage` is configurable for air-gapped registries that mirror the
  k8s-agent image under a different name/tag, **and to pin the shim** — see
  below.

#### Shim image: pin it in production

The `shimImage` default is the k8s-agent's *own* image, at the tag that matches
the agent's version: a `v0.7.0` agent defaults to
`ghcr.io/eirueimi/unified-cd-k8s-agent:v0.7.0`, so a released agent injects the
shim published with its own release by construction. An agent built any other
way (`make`, a plain `go build`; it reports `dev` or a `git describe` string)
defaults to `:latest`. The default is deliberately **not** digest-pinned: it
names this binary's own image, so a digest written into the source could only
ever be the previous release's, which would hard-code the version skew the
lockstep rule in [operations.md](operations.md) forbids. See the
`defaultShimImage` comment in `internal/k8sagent/config.go` for the full
reasoning.

The same rule now sets **every** unified-cd default image: `podImage` (the
runner) and `sidecarImage` default to a tag derived from the agent's version
too, and this section's guidance applies to all three. The one default that is
still a digest is the host agent's `pauseImage` (busybox); see [Default images
and the pause image digest](operations.md#default-images-and-the-pause-image-digest).

A tag is mutable, so the shipped defaults carry two risks an operator should
close explicitly:

1. **Mutable-tag exposure.** Whoever can write to the registry repository can
   move a tag. For `shimImage` that is fleet-wide code execution: the
   `ucd-shim` init container installs the `ucd-sh` binary that every
   subsequent step execs. For `sidecarImage` it is a credential-exfiltration
   path (see [Sidecar image](#sidecar-image)); for `podImage`, code execution
   in the primary container of every job that declares no container of its own.
2. **Version skew on a locally built agent.** `:latest` tracks the newest
   *published release*, not the agent you are running, so an agent built from a
   commit ahead of the last release injects an older shim, contrary to the
   lockstep requirement in [operations.md](operations.md). A released agent
   does not have this problem.

Both are closed by setting the field to the exact image you deployed, by
digest:

```yaml
# k8s-agent-config.yaml
shimImage: ghcr.io/eirueimi/unified-cd-k8s-agent@sha256:<digest of your agent image>
sidecarImage: ghcr.io/eirueimi/unified-cd-artifact-sidecar@sha256:<digest of the sidecar of the same release>
podImage: ghcr.io/eirueimi/unified-cd-runner@sha256:<digest of the runner of the same release>
```

Resolve each digest with:

```bash
docker buildx imagetools inspect ghcr.io/eirueimi/unified-cd-k8s-agent:<your tag>
docker buildx imagetools inspect ghcr.io/eirueimi/unified-cd-artifact-sidecar:<your tag>
docker buildx imagetools inspect ghcr.io/eirueimi/unified-cd-runner:<your tag>
```

Use the **top-level `Digest:`** of the multi-arch index, not one of the
per-platform digests under `Manifests:`. For the shim this is by construction
the lockstep-correct value, since it is the same image the agent itself runs.

A digest you set no longer follows the agent: **update all three whenever you
upgrade the agent**, or you will be running a shim, sidecar or runner from the
old release. The sidecar is the one that breaks visibly, because it speaks a
binary exec protocol to the agent (see [Sidecar image](#sidecar-image)). The
[migration note](migrations/2026-10-22-version-derived-default-images.md) lists
who is affected and what else changes with these defaults.

`/.ucd` is therefore a **reserved path**: a `podTemplate` that mounts
something else there is user error and fails loudly (an exec into that
container looks for `/.ucd/ucd-sh` and won't find it) the first time a step
runs.

### Keep-alive: `ucd-sh pause`

The primary `job` container's keep-alive is **unconditionally injected**,
discarding any `command`/`args` the container has set — a `podTemplate`
that sets `command`/`args` on the container named `job` has that command
silently overridden by the keep-alive on both backends; put the actual
workload in `steps:` instead. (Sidecar containers are unaffected: a
sidecar with no `command`/`args` still runs its own image entrypoint, and a
sidecar's own service command, e.g. `mysqld`, is never clobbered — only the
primary `job` container is forced.) The keep-alive argv itself
changed from `["sleep", "infinity"]` to `["/.ucd/ucd-sh", "pause"]`. This
applies uniformly, including the **bare `podImage` fallback** (no
`podTemplate` at all): that path routes through the same injection logic as
a `podTemplate`-defined `job` container, so it also gets the shim
keep-alive rather than being left uninjected. `ucd-sh pause` blocks until
SIGTERM/SIGINT, reaps zombie children while running as PID 1, and needs no
`sleep` binary in the image — the `scratch`/distroless keep-alive case that
`sleep infinity` could never satisfy.

### podTemplate container validation

`BuildPod` validates every `podTemplate` container before the Pod is sent
to the API server — matching validation the standard agent's claim pod
also performs (see [Job Reference: podTemplate container parity
notes](../user-guide/writing-jobs/isolation-and-containers.md#podtemplate-container-parity-notes-host-and-k8s)):

- **Every container must have a `name`.** An empty/missing `name` is a
  hard error at pod-build time (`podTemplate container at index N has no
  name`) rather than being sent to the API server and rejected late.
- **An `env` entry's `value` must be a string.** An unquoted number or
  boolean (`value: 8080`) fails Pod-spec decoding; quote it
  (`value: "8080"`).

### Job Pod field policy

A job's `podTemplate` can name any Kubernetes Pod field, and Roles in unified-cd are global: every
`developer` can write a job. So the k8s-agent filters what **job-supplied** Pod content may reach
the Kubernetes API server. By default it refuses, when it claims the run, a job whose own
`podTemplate` reaches a ServiceAccount, the node, a Kubernetes Secret, a PersistentVolumeClaim, or
a field the agent has not classified. Each category comes back through an explicit opt-in in the
agent config (`jobPodPolicy`, below).

**What is checked, and what is not.**

| Source | Checked? |
|---|---|
| `podTemplate.spec` (inline), including containers and volumes merged in from `uses:` templates | Yes, the whole decoded PodSpec |
| `podTemplate.override.containers` / `.volumes` | Yes, each patch container and volume |
| An override container's `volumeMounts` / `volumeDevices` that mount an **agent template** volume | Yes: the template volume is checked as if the job had declared it, so a job cannot add a container that mounts the operator's `hostPath` or `secret` volume |
| Agent templates (`podTemplates:` in this config) | **No.** They are operator-written and trusted |
| `podTemplate.name` alone, `reuse`, `cleanWorkspace`, `spec.volumes` (named volumes), `runsIn.image` | No: none of them lets a job write a Pod field |
| `podTemplate.workspace.mountPath` set by the **job**, and an absolute `mountPath` of a `spec.volumes` entry | Yes, by [the mount path rule](#job-chosen-mount-paths): these paths are mounted into every container, the artifact sidecar included |

The check runs once, right after the claim, before the pool, any volume lock and `BuildPod`. A
refused run therefore creates no Pod, takes no lock and cannot be served from an idle pooled Pod.
Moving the content into an agent template is always an alternative to opting in.

**What is denied by default.** Everything not listed as allowed is denied, and a PodSpec field or
volume type added by a later Kubernetes library bump is denied until the agent classifies it.

| Pod-level field | Default | Setting that allows it |
|---|---|---|
| `containers`, `initContainers`, `volumes`, `securityContext`, `restartPolicy`, `terminationGracePeriodSeconds`, `activeDeadlineSeconds`, `nodeSelector`, `affinity`, `tolerations`, `topologySpreadConstraints`, `hostname`, `subdomain`, `setHostnameAsFQDN`, `enableServiceLinks`, `os`, `hostUsers`, `resources` | allowed (nested content is checked) | none needed |
| `serviceAccountName`, `serviceAccount` | denied | `allowServiceAccounts: [<name>]` |
| `automountServiceAccountToken: true` | denied unless the Pod's effective account is allowed (`false` is always allowed) | `allowServiceAccounts: [<effective account>]` |
| `nodeName` | denied | `allowNodeName: true` |
| `hostNetwork`, `hostPID`, `hostIPC` set to `true` | denied | `allowHostAccess: true` |
| `imagePullSecrets` | denied | `allowImagePullSecrets: [<name>]` |
| `shareProcessNamespace`, `runtimeClassName`, `priorityClassName`, `priority`, `preemptionPolicy`, `schedulerName`, `readinessGates`, `overhead`, `schedulingGates`, `resourceClaims`, `hostnameOverride`, `schedulingGroup`, `hostAliases`, `dnsPolicy`, `dnsConfig` | denied | `allowPodSpecFields: [<field>]` |
| `ephemeralContainers` | denied; the API server rejects it on Pod create anyway | nothing |

| Container rule (`containers`, `initContainers`, override containers) | Default | Setting that allows it |
|---|---|---|
| `ports[].hostPort`, `ports[].hostIP` | denied | `allowHostAccess: true` |
| `livenessProbe` / `readinessProbe` / `startupProbe` / `lifecycle` hook `httpGet.host` or `tcpSocket.host` | denied | `allowHostAccess: true` |
| `env[].valueFrom.secretKeyRef`, `envFrom[].secretRef` | denied | `allowSecrets: [<name>]` |
| `configMapKeyRef`, `fieldRef`, `resourceFieldRef`, `fileKeyRef`, `configMapRef` | allowed | none needed |
| Mounting `sidecar-s3-credentials` or `sidecar-store-credential-token` | denied | nothing (see below) |

| Volume type | Default | Setting that allows it |
|---|---|---|
| `emptyDir`, `configMap`, `downwardAPI`, `image`, `projected` (its `configMap`, `downwardAPI`, `clusterTrustBundle` sources) | allowed | none needed |
| `projected` `secret` source, `secret` volume | denied | `allowSecrets: [<name>]` |
| `projected` `serviceAccountToken` / `podCertificate` source | denied unless the Pod's effective account is allowed | `allowServiceAccounts: [<effective account>]` |
| `persistentVolumeClaim` | denied | `allowPersistentVolumeClaims: [<claim>]` |
| `hostPath` | denied | `allowHostAccess: true` |
| `ephemeral`, `csi`, `nfs`, `iscsi`, `fc`, `rbd`, `cephfs`, `glusterfs`, `flexVolume`, `gitRepo`, `cinder`, `flocker`, `azureFile`, `azureDisk`, `awsElasticBlockStore`, `gcePersistentDisk`, `vsphereVolume`, `quobyte`, `photonPersistentDisk`, `portworxVolume`, `scaleIO`, `storageos` | denied | `allowVolumeTypes: [<type>]` |

| Security context (Pod level and container level) | Default | Setting that allows it |
|---|---|---|
| `runAsUser` (including `0`), `runAsGroup`, `runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation`, `supplementalGroups`, `supplementalGroupsPolicy`, `fsGroup`, `fsGroupChangePolicy`, `seLinuxChangePolicy`, `capabilities.drop` | allowed | none needed |
| `privileged: true` | denied | `allowPrivileged: true` |
| `capabilities.add` outside the `baseline` set (`AUDIT_WRITE`, `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `FSETID`, `KILL`, `MKNOD`, `NET_BIND_SERVICE`, `SETFCAP`, `SETGID`, `SETPCAP`, `SETUID`, `SYS_CHROOT`) | denied | `allowPrivileged: true` |
| `procMount` other than `Default`; `seLinuxOptions` with `user`/`role` or a `type` outside the `baseline` set; `sysctls` outside the safe set; `windowsOptions.hostProcess: true` | denied | `allowPrivileged: true` |
| `seccompProfile.type` or `appArmorProfile.type` of `Unconfined` (`RuntimeDefault` and `Localhost` are allowed) | denied | `allowUnconfinedProfiles: true` |

The default policy is meant to be a subset of Pod Security `baseline` for job content, plus the
unified-cd categories `baseline` does not cover (ServiceAccounts, Secrets, image pull Secrets,
PVCs, `nodeName`, the field allowlists). It does not go beyond `baseline` on container hardening:
`allowPrivilegeEscalation: true` and `runAsUser: 0` pass. Set the `restricted` level on the
namespace if you want those refused.

**The effective ServiceAccount.** For an inline spec it is `serviceAccountName`, else
`serviceAccount`, else `default`. For an override it is the **agent template's** account, because a
patch carries only containers and volumes. `allowServiceAccounts: [deployer]` therefore lets a job
project a token only into a Pod that really runs as `deployer`.

**Configuration.** A new top-level block in the k8s-agent config file. Every entry is optional, and
an absent block is the most restrictive policy. It is file-only: there are no environment variables
for it. See also the [configuration
reference](../reference/configuration.md#k8s-agent-config-fields).

```yaml
# k8s-agent-config.yaml
jobPodPolicy:
  # Named objects: list each name. There are no wildcards.
  allowServiceAccounts: []        # serviceAccountName / serviceAccount values a job may set; also gates
                                  # automountServiceAccountToken: true and projected serviceAccountToken /
                                  # podCertificate sources (checked against the Pod's effective account)
  allowSecrets: []                # Secrets a job may read: secretKeyRef, secretRef, secret volumes, projected
                                  # secret sources. May not name sidecarS3SecretName.
  allowImagePullSecrets: []       # imagePullSecrets a job may name (the kubelet pulls with the Secret; the
                                  # job cannot read it)
  allowPersistentVolumeClaims: [] # claimName values of persistentVolumeClaim volumes. May not name this
                                  # agent's own named-volume claims (<agentName>-<volume>).
  # Categories:
  allowHostAccess: false          # hostPath; hostNetwork / hostPID / hostIPC; hostPort / hostIP; probe and
                                  # lifecycle httpGet.host / tcpSocket.host
  allowPrivileged: false          # privileged; non-baseline capabilities.add; procMount; non-baseline
                                  # seLinuxOptions; unsafe sysctls; windowsOptions.hostProcess
  allowUnconfinedProfiles: false  # seccompProfile / appArmorProfile type Unconfined
  allowNodeName: false            # nodeName
  # Escape hatches for fields that are neither always allowed nor in a category:
  allowPodSpecFields: []          # e.g. [runtimeClassName, priorityClassName, hostAliases, dnsConfig]
  allowVolumeTypes: []            # e.g. [nfs, csi, ephemeral]
```

At startup the agent logs one line, `k8s: job pod policy`, with the effective opt-ins
(`allowed=none` when there are none).

**Startup validation.** The agent refuses to start (exit 1) on any of these, so a typo cannot
silently narrow or widen the policy. All but the first are reported as `invalid config:
jobPodPolicy: ...`:

- An unknown key under `jobPodPolicy:` (reported while loading the file, as `load config: ...:
  jobPodPolicy: ... field <key> not found`). The block is decoded strictly, although the rest of
  the config file is not.
- A name in `allowServiceAccounts`, `allowSecrets`, `allowImagePullSecrets` or
  `allowPersistentVolumeClaims` that is not a valid Kubernetes object name (a DNS-1123 subdomain).
  An entry containing `*` gets `wildcards are not supported; list each name`.
- `allowSecrets` naming `sidecarS3SecretName`:
  `allowSecrets: "<name>" is sidecarS3SecretName, the artifact sidecar's credentials, which job containers must never read`.
- `allowPersistentVolumeClaims` naming one of this agent's named-volume claims
  (`<agentName>-<volume>`):
  `allowPersistentVolumeClaims: "<claim>" is this agent's named volume "<volume>"; jobs reach it only through spec.volumes, which holds its lock`.
- An `allowPodSpecFields` or `allowVolumeTypes` entry that is not an opt-in name:
  `"<x>" is always allowed; remove it`, `"<x>" is governed by jobPodPolicy.<key>; use that instead`,
  `"<x>" cannot be allowed: it cannot be set when a Pod is created` (`ephemeralContainers`), or
  `"<x>" is not an opt-in PodSpec field; valid entries: ...` (the message lists the valid names).

**The run error.** A refused run fails immediately. The error is the first thing in the run's own
log (the "System" entry), the run finishes `Failed`, and nothing was executed. Every violation is
listed in one message, in a fixed order, each with the setting that would allow it:

```
k8s: job pod policy: the job's podTemplate sets 4 field(s) this agent does not allow: podTemplate.spec.serviceAccountName: selects ServiceAccount "deployer" (allow with jobPodPolicy.allowServiceAccounts: ["deployer"] in the k8s-agent config); podTemplate.spec.containers[1].securityContext.privileged (container "docker"): privileged: true (allow with jobPodPolicy.allowPrivileged: true in the k8s-agent config); podTemplate.spec.containers[2].env[0].valueFrom.secretKeyRef (container "db"): reads Secret "db-password" (allow with jobPodPolicy.allowSecrets: ["db-password"] in the k8s-agent config); podTemplate.spec.volumes[0].hostPath (volume "root"): a hostPath volume ("/" on the node) (allow with jobPodPolicy.allowHostAccess: true in the k8s-agent config). Agent-side podTemplates (podTemplates: in the k8s-agent config) are not subject to this policy; see "Job Pod field policy" in docs/operator-manual/kubernetes-integration.md
```

A violation that nothing can allow ends with `(no k8s-agent setting allows this)` instead. The
grep-able prefix is `k8s: job pod policy:`. For the job author's view, see [Run fails immediately
on Kubernetes: "k8s: job pod
policy: ..."](../troubleshooting/steps-and-execution.md#run-fails-immediately-on-kubernetes-k8s-job-pod-policy).

**Names that can never be allowed.**

- The artifact sidecar's credential volumes, `sidecar-s3-credentials` (`file` mode) and
  `sidecar-store-credential-token` (`broker` mode), can never be mounted by a job container. A
  `volumeMounts` or `volumeDevices` entry naming either is refused whatever the policy says.
- `allowSecrets` may not name `sidecarS3SecretName`, and `allowPersistentVolumeClaims` may not name
  a named-volume claim. The agent refuses to start if the config lists them. A named volume is
  reachable only through `spec.volumes`, behind its lock, and the sidecar's credentials must stay
  out of job containers.

**`hostAliases`, `dnsPolicy` and `dnsConfig` are opt-in.** Name resolution is a Pod-wide setting,
so it applies to the artifact sidecar as well as to the job containers. A job that could set it
would decide where the sidecar's connections to the controller's store-credentials broker and to
the object store go. Allow them only for jobs you trust, with `allowPodSpecFields: [hostAliases]`
(and so on), or put them in an agent template, which is not filtered.

**What the opt-in does not do.** Keeping these three fields out of a job's `podTemplate` stops a job
from using the PodSpec fields; it does not isolate the sidecar's network path from a job container.
Every container in the Pod shares one network namespace and the kubelet's single writable
`/etc/hosts`. A job container that runs as root (the `baseline` profile allows root) can edit that
`/etc/hosts` for the whole Pod, can listen on `127.0.0.1` on the port the sidecar's broker or
object-store client uses, and, because `baseline` keeps `NET_RAW`, can sniff the Pod's traffic.
The mitigations that work are:

- **Serve the controller (the store-credentials broker) over TLS** and point the agent at an
  `https://` URL. The shipped install reaches it over plain HTTP, so an on-Pod attacker who can
  redirect or sniff the connection sees the sidecar's token. With TLS and certificate verification
  the redirect or sniff yields nothing usable.
- **Run job containers as non-root with a read-only root filesystem** (`securityContext:
  runAsNonRoot: true`, `readOnlyRootFilesystem: true`, and `capabilities: {drop: [NET_RAW]}`) in
  your agent templates, so a job cannot rewrite `/etc/hosts` or sniff. This is operator policy in
  a template; the job Pod field policy does not impose it.

Treat both as recommendations to apply, not as something the policy already guarantees.

<a id="job-chosen-mount-paths"></a>

**Job-chosen mount paths.** The workspace `emptyDir` and every absolute `spec.volumes` entry are
mounted into every container of the Pod, and that includes the artifact sidecar. So the agent
refuses, whatever `jobPodPolicy` says, a run whose job-supplied `podTemplate.workspace.mountPath` or
absolute `spec.volumes[].mountPath` is not absolute, is not clean (`path.Clean` would change it), is
`/`, or is equal to, inside or above any of `/usr`, `/bin`, `/sbin`, `/lib`, `/lib32`, `/lib64`,
`/libx32`, `/etc`, `/proc`, `/sys`, `/dev`, `/run`, `/var/run`, `/.ucd`, `/usr/local/bin` or the
sidecar's credential mount directories. `/workspace` and `/home/runner/work` are fine. A relative
`spec.volumes` path resolves under the workspace and is covered by the workspace check. The check
runs on the path the agent will actually mount: a backslash is turned into `/` and the result
cleaned and, if relative, joined under the workspace before it is compared (`\usr\local\bin` is
`/usr/local/bin`), through the same helper the mount code uses. `apply` also rejects any backslash
in a `spec.volumes` or `workspace` `mountPath` (`mountPath must be a POSIX path`). A
`workspace.mountPath` in an agent template is not checked, and `apply` rejects a bad
`workspace.mountPath` up front. The agent also starts and execs the sidecar as
`/usr/local/bin/unified-sidecar`, by absolute path, so a mount cannot redirect a `PATH` lookup.

<a id="sidecar-filesystem-confinement"></a>

**The sidecar only reads and writes inside the workspace.** A job controls symlinks in the
workspace, but the sidecar follows them in its own filesystem, where its credential mounts and
trust store live. So the sidecar opens the workspace mount (the `--root` it is given on every
artifact and cache invocation) as a confined root and resolves every path beneath it: a symlink or
`..` that leads outside the workspace is never followed, on upload or on download and restore. An
`uploadArtifact` of `l/s3-credentials`, where `l` is a symlink to `/etc/unified-cd`, now fails
instead of archiving the credential, and a download or cache restore into a path below a symlink
that points outside the workspace fails instead of writing there. A symlink inside the workspace
that stays inside it keeps working as a path component. A symlink found while archiving a directory
is stored as a link entry and its target is not read. The sidecar and the agent must be upgraded
together, because the agent now passes `--root`.

**No step can target the sidecar.** Independently of the Pod policy, and on every agent type, a
step's `container:` can never be `unified-artifact` (the sidecar) or `ucd-shim` (the init
container, which has exited before any step runs). It is rejected when the job is applied, again
after `uses:` templates are resolved, and on the k8s-agent when the run is claimed and when the
exec is attempted. See [Reserved container name](#reserved-container-name) and the [migration
note](migrations/2026-10-26-k8s-job-pod-field-policy.md#reserved-step-targets).

**The shipped namespace enforces Pod Security `baseline`.** `manifests/base/namespace/namespace.yaml`
labels the `ci` namespace `pod-security.kubernetes.io/enforce: baseline` and
`pod-security.kubernetes.io/audit: restricted`. This is a second, independent layer: the agent
policy is the unified-cd-side floor, and admission is the cluster-side ceiling. There is no
`warn: restricted` label, because every agent-built Pod (shim init container, sidecar) violates
`restricted` and the agent log would get a warning on every Pod create. The audit annotation costs
nothing and tells you what a move to `restricted` would break. No version labels are set, so the
level follows the cluster's own `latest`. The `unified-cd` namespace that holds the controller and
the agent is not labelled. A namespace you create yourself carries no such labels; add them.

`baseline` is enforced by the API server whatever the agent config says, so it also refuses
**operator** templates that use `privileged: true`, `hostPath`, host namespaces, `Unconfined`
profiles and the like. The create fails with `k8s: failed to create Pod: pods "ucd-run-..." is
forbidden: violates PodSecurity "baseline:latest": ...`. If a template needs one of these (the
`golang` example's Docker-in-Docker container, rootless BuildKit), either relabel the job
namespace (`pod-security.kubernetes.io/enforce: privileged`) or, better, give those jobs a
separate agent with its own namespace. See [Run fails: violates PodSecurity
"baseline:latest"](../troubleshooting/steps-and-execution.md#run-fails-k8s-failed-to-create-pod-violates-podsecurity-baselinelatest).

**Recommended: switch off the `default` ServiceAccount's token.** A Pod that names no
`serviceAccountName` runs as the namespace's `default` ServiceAccount, and Kubernetes mounts that
account's token unless the account or the Pod opts out. The policy does not force
`automountServiceAccountToken: false` onto job Pods, because jobs that rely on the `default`
account's token would break silently; it checks only what a job *chooses*. If your jobs do not need
that token, opt the account out in the job namespace:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: default
  namespace: ci
automountServiceAccountToken: false
```

**Keep an admission policy for `serviceAccountName` as defence in depth.** Neither layer replaces
a cluster policy: Pod Security does not restrict `serviceAccountName`, and the agent policy is only
as good as its config. Keep ServiceAccounts with API permissions out of the job namespace, or deny
their use with an admission policy (Kyverno, Gatekeeper, or a `ValidatingAdmissionPolicy`).

**Existing deployments.** The policy and the namespace labels are breaking changes. See the
[migration note](migrations/2026-10-26-k8s-job-pod-field-policy.md).

---

## Artifacts and Cache

The k8s-agent supports `uploadArtifact`, `downloadArtifact`, and `cache` steps via an auto-injected sidecar container that talks **directly to S3** — object bytes never transit the controller.

### How it works

When a job pod is created, the agent automatically adds a sidecar container named `unified-artifact` to the pod, running the `unified-sidecar` binary, always invoked by its absolute path `/usr/local/bin/unified-sidecar` (a static, distroless image — no shell, `tar`, or `curl` inside it). The container is kept alive with `unified-sidecar idle`; individual transfers are dispatched into it via `exec` with an explicit argv (e.g. `unified-sidecar artifact upload --run <id> --name <name> --path <dir>`), never through a shell string. The sidecar shares the pod's workspace volume and reads/writes objects in the S3-compatible bucket configured for the agent.

Cache transfers additionally carry a `--job <qualifiedJobName>` argument (e.g. `unified-sidecar cache restore --key <key> --path <dir> --job team-a/build`), required and non-empty, so cache entries stay namespaced per job — see "Cache entries are namespaced per job" below.

Object key layout:

- Artifacts: `artifacts/{runID}/{name}.tar.gz`
- Cache: `caches/<base64url(sha256(qualifiedJobName))>/<base64url(sha256(key))>.tar.zst` (+ matching `.meta` for TTL/owner metadata) — unpadded, URL-safe base64 of each raw SHA-256 digest, not the hex digest itself. The job component namespaces every entry — see [Job Reference: Cache](../user-guide/writing-jobs/artifacts-and-cache.md#cache) for the security rationale and what this means for pre-existing cache entries.

Job-container steps (`run:` commands) are unaffected — the sidecar runs in its own container and is invisible to the main step execution.

The `unified-artifact` sidecar's own `exec` output (the transfers themselves) is streamed into the run's logs under its own "Sidecars" group entry (named `artifact`) in the run detail UI, the same as any user-declared `podTemplate` sidecar — see [Job Reference: Sidecar container logs](../user-guide/writing-jobs/isolation-and-containers.md#sidecar-container-logs). It no longer ships mixed into the first step's log stream.

**Cache** is best-effort: a `cache:` step restores at step time if a matching key exists, but a miss or restore error never fails the step. The matching save is deferred until the end of the run (after all stages complete, mirroring the standard agent's cache semantics) and is also best-effort — a save error is logged but never fails the run. **Artifacts are not best-effort** — a failed `uploadArtifact`/`downloadArtifact` transfer fails the step, same as the pre-existing k8s behavior.

### Reserved container name

The container name `unified-artifact` is **reserved**. A `podTemplate` must not define a container with that name. The agent returns a `BuildPod` error at job start if the name conflicts.

It is also never a valid step target: a step's `container: unified-artifact` (or `container: ucd-shim`) is rejected at apply time and refused by the k8s-agent at claim time, because the sidecar holds the object-store credentials. Only `job` and containers you declare in the `podTemplate` can be targeted. See [Job Pod field policy](#job-pod-field-policy).

### Sidecar image

The sidecar image is configurable via the agent's `sidecarImage` config field:

```yaml
# k8s-agent-config.yaml
# The default is derived from the agent's version (:v0.7.0 for a v0.7.0 agent,
# :latest for a locally built agent). Leave it unset to follow the agent; a
# literal tag pasted here pins the sidecar while the agent moves on.
# sidecarImage: ghcr.io/eirueimi/unified-cd-artifact-sidecar:<agent version>   # default
```

The default is a version-derived tag, not a digest. The sidecar is
auto-injected into every job and scope Pod and holds long-lived, bucket-scoped
S3 credentials, so a mutable tag would let a registry compromise exfiltrate
those credentials from every Pod in the fleet. If that matters to you, set
`sidecarImage` to a digest, as described under [Shim image: pin it in
production](#shim-image-pin-it-in-production), which covers all the default
images.

Pinning by digest closes the registry risk but not skew: the sidecar and the
k8s-agent talk over a binary exec protocol and must be upgraded in lockstep
(see [Operations: Upgrades](operations.md#upgrades)), so a digest left behind
across a k8s-agent upgrade pins the *wrong* sidecar. The version-derived default
does not have this problem: it moves with the agent. To confirm a running
pod's pair actually matches, ask both binaries:

```bash
kubectl exec <pod> -c unified-artifact -- unified-sidecar version
kubectl exec <pod> -- /k8s-agent --version
```

Both print `dev` unless the image was built from a release tag, and nothing
compares them automatically — this is an operator check, not an enforced
constraint.

**What skew costs now.** When a job archives an *ancestor* of a named volume's mount point (`path:
.`, say), the agent passes the sidecar one `--exclude <path>` per volume, so the volume's contents
stay out of the archive (or, for a download, are not extracted into); the flag exists on
`artifact upload`, `artifact download`, `cache save` and `cache restore`. The
agent adds it **only** in that case, so an older sidecar image that does not know the flag works
for every other job and exits 2 (`flag provided but not defined: -exclude`) for this one: the
artifact upload fails, and the cache reads as "not restored". Roll the sidecar image and the
agent together. See [the troubleshooting
entry](../troubleshooting/artifacts-and-storage.md#artifact-upload-fails-or-a-cache-is-never-restored-flag-provided-but-not-defined-exclude).

### S3 credentials (required)

This section describes the default (`sidecarS3SecretMode: env`, and `file`,
its rotation-capable sibling). A third mode, `broker`, needs **no Secret at
all** — see "`sidecarS3SecretMode: broker`" below if you would rather not
manage one.

The operator must create a Kubernetes `Secret` **in the namespace the agent's `namespace:` config field points at** — the namespace job Pods are created in, which in the shipped manifests is `ci`, **not** the `unified-cd` namespace the agent Deployment itself runs in.

This distinction is the single most common way this setup fails. The sidecar is a container inside the job Pod, and the agent attaches the Secret to it with a `LocalObjectReference` (`envFrom.secretRef`), which has **no namespace field** — Kubernetes always resolves it in the Pod's own namespace. A Secret sitting in the agent's namespace is invisible to it.

It is also why the controller's S3 credentials are not enough on their own: logs travel agent → controller → S3, so the controller's configuration covers them, but Kubernetes artifact and cache transfers go from the sidecar **straight to S3**, and need their own copy of the credentials in the job namespace.

The Secret carries the same S3 env vars used by the controller/standard agent:

```
UNIFIED_S3_ENDPOINT     # required
UNIFIED_S3_BUCKET       # required
UNIFIED_S3_KEY          # required
UNIFIED_S3_SECRET       # required
UNIFIED_S3_USE_SSL      # optional, bool (default: derived from endpoint scheme)
UNIFIED_S3_REGION       # optional
```

Point the agent at it via `sidecarS3SecretName` in the config file:

```yaml
# k8s-agent-config.yaml
namespace: ci                          # job Pods are created here
sidecarS3SecretName: unified-cd-s3-creds   # ...so this Secret must exist HERE too
```

The Secret is injected into the sidecar container only, via `envFrom`.

**Unset and "named but missing" are not the same failure — do not treat them as interchangeable:**

| `sidecarS3SecretName` | What happens |
| --- | --- |
| Unset | The Pod starts normally. The sidecar simply has no S3 credentials, so **artifact steps fail** and **cache steps no-op** with one loud per-run warning. Everything else in the job runs. The agent detects a doomed artifact step at claim time and fails the run immediately, before creating a Pod, naming the offending steps — but only for a step that would actually have run and actually have failed the run: `continueOnError: true` steps are exempt, and `if:`-guarded steps are warned about rather than failed, since the guard cannot be evaluated before the run starts. |
| Set, but no such Secret in the **job Pod's** namespace | **Every job breaks, artifact-related or not.** `envFrom.secretRef` is not marked `optional: true`, so the kubelet cannot configure the sidecar container at all and fails the Pod with `CreateContainerConfigError: secret "…" not found`. The Pod never reaches `Running`, so no step of any kind executes. |

The second row is what an operator hits after creating the Secret in the agent's namespace instead of the job namespace. The agent surfaces the kubelet's reason and message in the run's failure (see [Troubleshooting](../troubleshooting/artifacts-and-storage.md#job-pods-never-start-with-createcontainerconfigerror)), so the `secret "…" not found` text appears in the run itself rather than only under `kubectl describe pod`.

### S3 credential delivery mode: `sidecarS3SecretMode`

By default the sidecar's S3 Secret reaches it the way described above: `envFrom.secretRef`, injected once when the container is created. That has a consequence worth naming explicitly — **a Secret consumed via `envFrom` is snapshotted into the container's environment at creation and never updates.** If you rotate the Secret's value, every already-running sidecar keeps using the old one until its Pod is replaced. There is no way to push a refreshed or short-lived credential into a live Pod under this mode; the shape doesn't support it.

`sidecarS3SecretMode: file` is the opt-in alternative that removes that ceiling:

```yaml
# k8s-agent-config.yaml
sidecarS3SecretName: unified-cd-s3-creds
sidecarS3SecretMode: file   # default: env
```

(Env override: `UNIFIED_K8S_SIDECAR_S3_SECRET_MODE`.)

In `file` mode the same-named Secret is instead mounted as a **volume** into the sidecar container, and the sidecar is told where to find it via `UNIFIED_S3_CREDENTIAL_FILE`, not `envFrom`. A Secret mounted as a volume **is** kept up to date by the kubelet — rewriting it eventually reaches the running Pod. This is the mode a rotated or refreshed credential needs; `env` mode cannot deliver one no matter how the Secret is managed.

**`file` mode expects a differently-shaped Secret than `env` mode.** `envFrom` needs `UNIFIED_S3_KEY` and `UNIFIED_S3_SECRET` as separate top-level Secret keys, one per env var. `file` mode instead expects a single key named `credentials`, whose value is:

```
UNIFIED_S3_KEY=<your key>
UNIFIED_S3_SECRET=<your secret>
```

(the same two variables, just both inside one value). Switching a Secret from one mode to the other means re-authoring it, not just flipping the config field — the sidecar will not find `UNIFIED_S3_KEY`/`UNIFIED_S3_SECRET` as top-level keys of a `file`-mode Secret, or the `credentials` key in an `env`-mode one.

The volume is mounted `optional: true` and `defaultMode: 0400`, mirroring how the controller's own KEK is mounted. `optional: true` matters independently of rotation: unlike `envFrom.secretRef` (which is never optional and makes a missing or misnamed Secret fail the **whole Pod** with `CreateContainerConfigError` — see the table above), a missing Secret under `file` mode simply leaves the mount empty and the sidecar without credentials, degrading the same way as leaving `sidecarS3SecretName` unset entirely (artifact steps fail, cache steps no-op, everything else in the job runs).

**What `file` mode does not do.** It is a delivery mechanism only — it does not by itself produce short-lived or automatically-rotating credentials. Nothing in this project rotates the Secret's value for you; an operator (or an external secret-rotation tool) still has to rewrite it, exactly as today, just with a mode that can actually deliver the new value to a running Pod. Building automatic issuance — an agent-projected credential, or a cloud-issued short-lived one — is future work; see the `2026-08-26-sidecar-credential-delivery-design.md` design spec, §5.5, for the seam this implements and why it was built ahead of that decision.

`env` remains the default: existing deployments that never set `sidecarS3SecretMode` see no change in behaviour.

### `sidecarS3SecretMode: broker` — no operator-created Secret at all

`env` and `file` both still require the operator to create an S3 Secret in
every job namespace, keep it in step with the controller's own credentials,
and — the trap §3 of the design spec below is written about — get it into
the **job Pod's** namespace, not the agent's. `broker` removes that Secret
entirely:

```yaml
# k8s-agent-config.yaml
namespace: ci
sidecarS3SecretMode: broker   # default: env — no sidecarS3SecretName needed
```

(Env override: `UNIFIED_K8S_SIDECAR_S3_SECRET_MODE=broker`.)

In this mode the agent adds a **projected ServiceAccount token volume** to
the job Pod — a Kubernetes primitive the kubelet mints directly from the
Pod spec, needing no RBAC beyond what writing that Pod spec already
requires — mounted into the sidecar container only, with its own audience
(`unified-cd-store-credentials`, distinct from the agent's own enrollment
audience). The sidecar presents that token to the controller's
`POST /api/v1/store-credentials`, which verifies it with a `TokenReview`
(the same mechanism used for Kubernetes agent enrollment, against a
different audience so neither kind of token can be used as the other) and,
if the Pod's namespace and ServiceAccount are on the controller's
`agentAuth.kubernetesStoreCredentialPolicies` allowlist for that cluster,
returns object-store credentials.

**What this does and does not change:**

- **No Secret in any job namespace.** The operator's per-namespace Secret,
  and the trap of creating it in the wrong one, both disappear.
- **No new agent RBAC.** The agent adds a volume to a Pod spec it already
  writes; it still never touches a Secret and still cannot read or write
  one.
- **The credential returned today is the controller's own, passed through
  unscoped — not per-run, not short-lived.** Every Pod authorized by the
  allowlist gets the identical credential the controller itself uses, for
  as long as the controller's own credential is valid. This is a real,
  deliberate limitation, not an oversight: scoping the credential to a run's
  object-store prefix needs STS support (`AssumeRoleWithWebIdentity`) that
  is not available on every store this project supports — notably Garage,
  which the bundled evaluation manifests use, and whose support for it is
  unconfirmed. Passthrough works everywhere. The response shape already
  carries an expiry and a session token, so a future scoped or short-lived
  credential is a change of what the controller returns, not a wire-format
  change every sidecar has to catch up to — but that work has not landed
  yet. Do not treat `broker` mode as least-privilege per run: today it is
  the same bucket-wide blast radius as `env`/`file`, delivered without a
  Secret.
- **The data path is unchanged.** Artifact and cache bytes still go
  straight from the sidecar to the object store; only the one-time
  credential fetch at the start of a transfer passes through the
  controller.

The controller needs `agentAuth.kubernetesStoreCredentialPolicies` configured
for the same cluster the k8s-agent enrolls against, naming the job Pod's
namespace(s) and ServiceAccount(s) — a separate list from
`kubernetesEnrollmentPolicies`, because the two authorize different
identities: the agent's own (e.g. `unified-cd`/`unified-cd-k8s-agent`) versus
the job Pod's (e.g. `ci`/whatever ServiceAccount runs your jobs, `default`
if none is set). See `manifests/install/controller-config-patch.yaml` for a
worked example — the bundled `install.yaml` evaluation manifests use this
mode by default (`core-install.yaml` still defaults to `env`).

See `docs/superpowers/specs/2026-08-26-sidecar-credential-delivery-design.md`
§5.6 for the full design and the reasoning behind passthrough-only for now.

### Security note / threat model

Bucket-scoped S3 credentials reach the sidecar one of three ways (`env`, `file`, or `broker`; see above), but in every mode they land in the **sidecar container's** environment or a mount only — **by default** the job container never sees them (container-boundary isolation, the same trust boundary Argo Workflows and Tekton use for their artifact sidecars/init-containers). Six things keep that true: the [Job Pod field policy](#job-pod-field-policy) refuses a job's `secretKeyRef` / `secret` volume for any Secret the operator has not listed (and `allowSecrets` can never name `sidecarS3SecretName`) and refuses any mount of the agent-injected credential volumes; it refuses a job-chosen workspace or volume mount path over the sidecar's binaries, libraries, configuration or credential mounts (checked on the path as the agent will mount it, and the agent runs the sidecar by absolute path), the sidecar confines all of its file access to the workspace so a job-planted symlink cannot make it read its own credentials or write over its own files, and it denies `hostAliases`, `dnsPolicy` and `dnsConfig` unless the operator opts in, so a job cannot set Pod-wide name resolution (this does not stop a root job container from editing the shared `/etc/hosts` or listening in the shared network namespace; see "What the opt-in does not do" above); no step can target the sidecar (`container: unified-artifact` is rejected at apply time, after `uses:` resolution, and on the k8s-agent at claim and exec time); and the default policy does not allow `shareProcessNamespace`. The remaining exceptions are operator opt-ins, and none should be set on an agent that runs untrusted jobs: `jobPodPolicy.allowPodSpecFields: [shareProcessNamespace]` lets the job container read the sidecar's `/proc/<pid>/environ` and root filesystem and so defeats the container boundary, and `[hostAliases, dnsPolicy, dnsConfig]` let a job set Pod-wide name resolution that redirects the sidecar's broker and object-store connections. Even without them, a job container running as root can edit the Pod's shared `/etc/hosts` or listen and sniff in the shared network namespace; the real mitigations are serving the broker over TLS (the shipped install uses plain HTTP) and running job containers non-root with `readOnlyRootFilesystem` and `NET_RAW` dropped. Under all three modes the credential is today the SAME long-lived, bucket-scoped one: any workload able to exec into the `unified-artifact` container (or, under `env`/`file`, read the Secret directly if RBAC allows `get`/`list` on Secrets in the namespace) can read/write the whole bucket for as long as that credential is valid.

This is comparable to how most CI systems hand artifact/cache sidecars static bucket credentials, but it is **not** least-privilege per run, under any of the three modes. `broker` (§5.6 above) removes the per-namespace Secret and is the mechanism a future short-lived or per-pod-scoped credential would be built on — bare-metal, EKS and GKE alike, unlike IAM Roles for Service Accounts (IRSA, EKS-only) or Workload Identity (GKE-only) — but it does not deliver that scoping today; see the `broker` section above for exactly what it does and does not change. Until a scoped credential ships, restrict RBAC `get`/`list`/`watch` on Secrets and `pods/exec` in the agent's namespace to trusted operators under `env`/`file`, and restrict `agentAuth.kubernetesStoreCredentialPolicies` to only the namespaces/ServiceAccounts that actually run jobs under `broker`.

---

## RBAC example

Minimum permissions required for k8s-agent to operate. `manifests/base/k8s-agent/rbac.yaml` ships
a Role that is a superset of this one (it keeps a `delete` verb on PVCs from before named volumes, which the agent does not use, and adds `update` and `patch` on Pods, which `podTemplate.reuse`
needs to mark a finished Pod idle; without them reuse silently never happens).

**When you add `volumes` to an agent, roll the Role first and the agent image second.** The agent
reads, lists and creates its PVCs at startup and updates them to take and release an exclusive
volume's lock. The Role that shipped before named volumes granted only `create` and `delete` on
`persistentvolumeclaims`, so an agent started against it fails immediately with a `forbidden`
error on `get`. The agent never deletes a volume's PVC itself. The `persistentvolumeclaims` rule
below is needed only when the agent config declares `volumes`.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: ci
  name: unified-cd-k8s-agent
rules:
  - apiGroups: [""]
    resources: ["pods", "pods/exec", "pods/log"]
    verbs: ["create", "get", "list", "delete", "watch"]
  - apiGroups: [""]
    resources: ["persistentvolumeclaims"]
    # Only needed when the agent config declares `volumes`: the agent reads and
    # creates its PVCs, and takes/releases an exclusive volume by updating the
    # claim's `unified-cd/volume-held-by` annotation. It never deletes a PVC.
    verbs: ["create", "get", "list", "update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  namespace: ci
  name: unified-cd-k8s-agent
subjects:
  - kind: ServiceAccount
    name: unified-cd-k8s-agent
    namespace: ci
roleRef:
  kind: Role
  name: unified-cd-k8s-agent
  apiGroup: rbac.authorization.k8s.io
```
