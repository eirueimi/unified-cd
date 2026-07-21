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
agent (`cmd/agent`); job steps run inside a Pod instead of locally. Orchestration itself is now a
single shared implementation (`internal/agent`'s `RunClaim`, driven through the `ExecBackend`
seam) — only the execution backend differs per agent. The remaining intentional differences are:

- **Execution order** — `matrix:`/`foreach:` combinations and `parallel:` groups run
  **sequentially** inside the Pod (the standard agent runs them in parallel goroutines).
- **`container:`** — supported on both agents as the canonical way to target a named
  `podTemplate` container. On k8s it execs into the named container of the job Pod. On
  the standard agent it execs into the corresponding container of the claim pod (see
  [Job Isolation: `native` and the claim
  pod](jobs.md#job-isolation-native-and-the-claim-pod)); a sidecar's `command`/`args`
  now match standard Kubernetes/OCI ENTRYPOINT/CMD semantics on **both** backends —
  see [Host container command/args
  semantics](#host-container-commandargs-semantics) below for the full truth table
  and per-runtime support matrix. Other host-unsupported `podTemplate` fields (a PVC
  workspace, extra pod-spec, `volumeMounts`, or non-literal env) are ignored with a
  WARN rather than applied. Unlike k8s, the standard agent's claim-pod containers
  share one network namespace (via the pause container), so — unlike the old MVP
  single-container form this replaces — sidecars **are** reachable at `localhost` from
  every claim-pod container, matching k8s.
- **Resource `requests`** (`podTemplate.spec.containers[].resources.requests`) — applied
  only here (docker/podman/nerdctl have no request concept). The standard agent maps
  `resources.limits` only and logs one WARN when `resources.requests` is present
  (`podTemplate container resources.requests is not supported on the host agent
  ... and is ignored; use resources.limits or route to a Kubernetes agent`).
- **`native: true`** — host-only. A `native: true` job claimed by the k8s-agent fails the
  run immediately with a clear error; route native jobs away from k8s-agents (and to host
  agents) via `agentSelector`.
- **Drain window** — on shutdown (SIGTERM/rollout) the k8s agent stops claiming immediately
  but lets in-flight runs keep going, same as the standard agent's `--drain-timeout`; see
  [Resilience & concurrency](#resilience--concurrency) below. Any run still in flight when the
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

The install manifests (`manifests/install.yaml`, `manifests/core-install.yaml`, `manifests/agent-only.yaml`) default the `unified-cd-k8s-agent` Deployment to `replicas: 2`, running active-active; see [Agent Redundancy](high-availability.md#agent-redundancy) in the HA guide for why this is safe.

---

## Resilience & concurrency

Three config fields (full reference: [Configuration: K8s Agent config
fields](configuration.md#k8s-agent-config-fields)) bound how the k8s-agent behaves under
scheduling pressure and during shutdown:

| Field (yaml) | Env override | Default | Behavior |
|---|---|---|---|
| `podStartTimeout` | `UNIFIED_K8S_POD_START_TIMEOUT` | `5m` | Bounds how long the agent waits for a run Pod to reach `Running` before failing the run. Without this, an unschedulable or `ImagePullBackOff` Pod would wedge the run forever — under `RestartPolicy: Never` a stuck-Pending Pod never transitions to `Failed` on its own. The wait also aborts early (without overriding the controller's status) if the run is already terminal at the controller. |
| `drainTimeout` | `UNIFIED_K8S_DRAIN_TIMEOUT` | `0` (wait indefinitely) | On SIGTERM/rollout, the agent stops claiming new runs immediately but lets in-flight runs keep going — heartbeats keep beating throughout drain so a draining run isn't reaped as stuck — until they finish or `drainTimeout` elapses, whichever is first. `0`/unset never forces a cutoff. Parity with the standard agent's `--drain-timeout`. |
| `maxConcurrent` | — | `100` | Max simultaneous job Pods, enforced by a semaphore around the claim loop. `0`/unset → `100` (raised from the previous default of `5`). A **negative** value (e.g. `-1`) removes the agent-side cap entirely — concurrency is then bounded only by cluster scheduling/quota. A positive value is an exact concurrency bound. |

Env vars, where present, override the config-file value (see [Configuration: Priority
Order](configuration.md#priority-order) — CLI flags still win over both, but these fields have
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
   argv — see [Job Reference: Shell (`shell:`)](jobs.md#shell-shell))
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
table](jobs.md#the-default-the-ucd-sh-shim) for exactly what the default
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
- `shimImage` is configurable specifically for air-gapped registries that
  mirror the k8s-agent image under a different name/tag.

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
notes](jobs.md#podtemplate-container-parity-notes-host-and-k8s)):

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
- Cache: `caches/<base64url(sha256(qualifiedJobName))>/<base64url(sha256(key))>.tar.zst` (+ matching `.meta` for TTL/owner metadata) — unpadded, URL-safe base64 of each raw SHA-256 digest, not the hex digest itself. The job component namespaces every entry — see [Job Reference: Cache](jobs.md#cache) for the security rationale and what this means for pre-existing cache entries.

Job-container steps (`run:` commands) are unaffected — the sidecar runs in its own container and is invisible to the main step execution.

The `unified-artifact` sidecar's own `exec` output (the transfers themselves) is streamed into the run's logs under its own "Sidecars" group entry (named `artifact`) in the run detail UI, the same as any user-declared `podTemplate` sidecar — see [Job Reference: Sidecar container logs](jobs.md#sidecar-container-logs). It no longer ships mixed into the first step's log stream.

**Cache** is best-effort: a `cache:` step restores at step time if a matching key exists, but a miss or restore error never fails the step. The matching save is deferred until the end of the run (after all stages complete, mirroring the standard agent's cache semantics) and is also best-effort — a save error is logged but never fails the run. **Artifacts are not best-effort** — a failed `uploadArtifact`/`downloadArtifact` transfer fails the step, same as the pre-existing k8s behavior.

### Reserved container name

The container name `unified-artifact` is **reserved**. A `podTemplate` must not define a container with that name. The agent returns a `BuildPod` error at job start if the name conflicts.

### Sidecar image

The sidecar image is configurable via the agent's `sidecarImage` config field:

```yaml
# k8s-agent-config.yaml
sidecarImage: ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest   # default
```

### S3 credentials (required)

The operator must create a Kubernetes `Secret` in the agent's namespace with the same S3 env vars used by the controller/standard agent:

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
sidecarS3SecretName: unified-cd-s3-creds
```

The Secret is injected into the sidecar container only, via `envFrom`. If `sidecarS3SecretName` is unset (or the named Secret doesn't exist), the sidecar has no S3 credentials: artifact steps fail loudly (this is an operator misconfiguration, not a transient error), and cache steps silently no-op (best-effort — a step never fails because the cache is unavailable).

### Security note / threat model

Bucket-scoped S3 credentials are mounted into the **sidecar container's** environment only, via the Kubernetes Secret's `envFrom` — the job container never sees them (container-boundary isolation, the same trust boundary Argo Workflows and Tekton use for their artifact sidecars/init-containers). The credentials are long-lived, bucket-scoped static keys, not per-run or per-pod scoped; any workload able to exec into the `unified-artifact` container (or read the Secret directly, if RBAC allows `get`/`list` on Secrets in the namespace) can read/write the whole bucket for as long as the Secret is valid.

This is comparable to how most CI systems hand artifact/cache sidecars static bucket credentials, but it is **not** least-privilege per run. A planned hardening is to move to short-lived, per-pod credentials via IAM Roles for Service Accounts (IRSA) on EKS or an equivalent Workload Identity / STS-assumed-role mechanism on other clouds, so the sidecar authenticates via a projected service-account token instead of a static Secret. Until then, restrict RBAC `get`/`list`/`watch` on Secrets and `pods/exec` in the agent's namespace to trusted operators.

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
