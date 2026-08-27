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
  │  volume: /workspace     │← emptyDir or PVC                    │
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
  standard agent. A PVC `workspace` is the one exception that still degrades instead
  of routing: it becomes a per-claim bind mount on the standard agent, by design. An
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
  same as `resources.requests`.
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
# to the k8s-agent's own image, which ships /ucd-sh at its root.
# shimImage: ghcr.io/eirueimi/unified-cd-k8s-agent:latest

# kubeconfig omitted → uses InClusterConfig if running inside the cluster,
#                       or ~/.kube/config if running outside
# kubeconfig: /path/to/kubeconfig

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
      mountPath: /workspace
      # specifying storageClassName causes an ephemeral PVC to be created automatically
      pvc:
        storageClassName: standard
        storageRequest: 5Gi
        accessMode: ReadWriteOnce
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

### Pattern 4: Pod reuse (build cache)

With `reuse: true`, the Pod is returned to a pool after the run and reused by the next run.
Build caches can accumulate in `/workspace`.

```yaml
spec:
  podTemplate:
    name: golang
    reuse: true
    cleanWorkspace: false   # default; set true to wipe /workspace before each run
    workspace:
      pvc:
        claimName: go-build-cache   # use an existing PVC for persistence
  steps:
    - name: download-deps
      run: |
        if [ ! -d /workspace/vendor ]; then
          go mod vendor
        fi
    - name: build
      run: go build ./...
```

---

## Workspace (`/workspace`) behavior

| Configuration | Behavior |
|---------------|----------|
| `workspace` not set | `emptyDir` (temporary, deleted when the Pod is deleted) |
| `pvc.storageClassName` set | An ephemeral PVC is created and deleted automatically |
| `pvc.claimName` set | An existing PVC is mounted (combine with `reuse: true` for persistent cache) |

All containers in the Pod mount the same path (`mountPath`), so files are shared between containers.

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

Unlike `sidecarImage` and `podImage`, the `shimImage` default is **not**
digest-pinned: it is the floating tag
`ghcr.io/eirueimi/unified-cd-k8s-agent:latest`. This is deliberate — the
default names the k8s-agent's *own* image, so a digest written into the source
could only ever be the previous release's, which would hard-code the version
skew the lockstep rule below forbids. See the `defaultShimImage` comment in
`internal/k8sagent/config.go` for the full reasoning.

The consequence is that the shipped default carries two risks an operator
should close explicitly:

1. **Version skew.** `:latest` tracks the newest *published release*, not the
   agent you are running. An agent built from a commit ahead of the last
   release injects an older shim, contrary to the lockstep requirement in
   [operations.md](operations.md). If no release has been published for a
   while, that gap can be large.
2. **Mutable-tag exposure.** The `ucd-shim` init container installs the
   `ucd-sh` binary that every subsequent step execs, so a registry compromise
   of a floating tag is fleet-wide code execution.

Both are closed by setting `shimImage` to the exact image you deployed the
agent from, by digest:

```yaml
# k8s-agent-config.yaml
shimImage: ghcr.io/eirueimi/unified-cd-k8s-agent@sha256:<digest of your agent image>
```

Resolve the digest with:

```bash
docker buildx imagetools inspect ghcr.io/eirueimi/unified-cd-k8s-agent:<your tag>
```

This is by construction the lockstep-correct value, since it is the same image
the agent itself runs. Update it whenever you upgrade the agent.

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

---

## Artifacts and Cache

The k8s-agent supports `uploadArtifact`, `downloadArtifact`, and `cache` steps via an auto-injected sidecar container that talks **directly to S3** — object bytes never transit the controller.

### How it works

When a job pod is created, the agent automatically adds a sidecar container named `unified-artifact` to the pod, running the `unified-sidecar` binary (a static, distroless image — no shell, `tar`, or `curl` inside it). The container is kept alive with `unified-sidecar idle`; individual transfers are dispatched into it via `exec` with an explicit argv (e.g. `unified-sidecar artifact upload --run <id> --name <name> --path <dir>`), never through a shell string. The sidecar shares the pod's workspace volume and reads/writes objects in the S3-compatible bucket configured for the agent.

Cache transfers additionally carry a `--job <qualifiedJobName>` argument (e.g. `unified-sidecar cache restore --key <key> --path <dir> --job team-a/build`), required and non-empty, so cache entries stay namespaced per job — see "Cache entries are namespaced per job" below.

Object key layout:

- Artifacts: `artifacts/{runID}/{name}.tar.gz`
- Cache: `caches/<base64url(sha256(qualifiedJobName))>/<base64url(sha256(key))>.tar.zst` (+ matching `.meta` for TTL/owner metadata) — unpadded, URL-safe base64 of each raw SHA-256 digest, not the hex digest itself. The job component namespaces every entry — see [Job Reference: Cache](../user-guide/writing-jobs/artifacts-and-cache.md#cache) for the security rationale and what this means for pre-existing cache entries.

Job-container steps (`run:` commands) are unaffected — the sidecar runs in its own container and is invisible to the main step execution.

The `unified-artifact` sidecar's own `exec` output (the transfers themselves) is streamed into the run's logs under its own "Sidecars" group entry (named `artifact`) in the run detail UI, the same as any user-declared `podTemplate` sidecar — see [Job Reference: Sidecar container logs](../user-guide/writing-jobs/isolation-and-containers.md#sidecar-container-logs). It no longer ships mixed into the first step's log stream.

**Cache** is best-effort: a `cache:` step restores at step time if a matching key exists, but a miss or restore error never fails the step. The matching save is deferred until the end of the run (after all stages complete, mirroring the standard agent's cache semantics) and is also best-effort — a save error is logged but never fails the run. **Artifacts are not best-effort** — a failed `uploadArtifact`/`downloadArtifact` transfer fails the step, same as the pre-existing k8s behavior.

### Reserved container name

The container name `unified-artifact` is **reserved**. A `podTemplate` must not define a container with that name. The agent returns a `BuildPod` error at job start if the name conflicts.

### Sidecar image

The sidecar image is configurable via the agent's `sidecarImage` config field:

```yaml
# k8s-agent-config.yaml
# The default is digest-pinned; the tag is kept only for readability.
sidecarImage: ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest@sha256:5e30d747d7ec954a88d84f4f7a8b5ac5c4b69d152555b80e253e7a0938eb14dd   # default
```

Keep the `@sha256:` digest if you override this. The sidecar is auto-injected
into every job and scope Pod and holds long-lived, bucket-scoped S3
credentials, so a mutable tag would let a registry compromise exfiltrate those
credentials from every Pod in the fleet.

Pinning by digest closes the registry risk but not skew: the sidecar and the
k8s-agent talk over a binary exec protocol and must be upgraded in lockstep
(see [Operations: Upgrades](operations.md#upgrades)), so a digest left behind
across a k8s-agent upgrade pins the *wrong* sidecar. To confirm a running
pod's pair actually matches, ask both binaries:

```bash
kubectl exec <pod> -c unified-artifact -- unified-sidecar version
kubectl exec <pod> -- /k8s-agent --version
```

Both print `dev` unless the image was built from a release tag, and nothing
compares them automatically — this is an operator check, not an enforced
constraint.

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

Bucket-scoped S3 credentials reach the sidecar one of three ways (`env`, `file`, or `broker`; see above), but in every mode they land in the **sidecar container's** environment or a mount only — the job container never sees them (container-boundary isolation, the same trust boundary Argo Workflows and Tekton use for their artifact sidecars/init-containers). Under all three modes the credential is today the SAME long-lived, bucket-scoped one: any workload able to exec into the `unified-artifact` container (or, under `env`/`file`, read the Secret directly if RBAC allows `get`/`list` on Secrets in the namespace) can read/write the whole bucket for as long as that credential is valid.

This is comparable to how most CI systems hand artifact/cache sidecars static bucket credentials, but it is **not** least-privilege per run, under any of the three modes. `broker` (§5.6 above) removes the per-namespace Secret and is the mechanism a future short-lived or per-pod-scoped credential would be built on — bare-metal, EKS and GKE alike, unlike IAM Roles for Service Accounts (IRSA, EKS-only) or Workload Identity (GKE-only) — but it does not deliver that scoping today; see the `broker` section above for exactly what it does and does not change. Until a scoped credential ships, restrict RBAC `get`/`list`/`watch` on Secrets and `pods/exec` in the agent's namespace to trusted operators under `env`/`file`, and restrict `agentAuth.kubernetesStoreCredentialPolicies` to only the namespaces/ServiceAccounts that actually run jobs under `broker`.

---

## RBAC example

Minimum permissions required for k8s-agent to operate:

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
    verbs: ["create", "delete"]
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
