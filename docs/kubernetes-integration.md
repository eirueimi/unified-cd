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
- **`runsIn.container`** — supported on both agents. On k8s it execs into the named
  container of the job Pod. On the standard agent it provisions a container from the
  same `podTemplate.spec.containers` entry, bind-mounting the job workspace into it so
  cache/artifacts/outputs are shared with the rest of the run; see the
  [`runsIn` field reference](resources.md#runsincontainer-on-the-standard-agent-mvp-limits)
  for its single-container MVP limits (no sidecar networking; host-unsupported
  `podTemplate` fields such as a PVC workspace, extra pod-spec, `command`, or non-literal
  env are ignored with a WARN).
- **`runsIn.resources.requests`** — applied only here (docker has no request concept; the
  standard agent maps limits only).
- **No drain window** — on shutdown the k8s agent stops immediately (in-flight runs are
  recovered by the startup reconcile / stuck-run reaper); the standard agent drains in-flight
  runs up to `--drain-timeout`.

Feature parity between the two agents is enforced by the shared conformance suite
(`internal/paritycases`) — new DSL behavior must pass identical expectations on both agents.

---

## Setup

### 1. Config file

Create `k8s-agent-config.yaml`:

```yaml
# Master server URL and agent token
server: http://unified-cd-master:8080
token: your-agent-token

agentId: k8s-agent-1
labels:
  - kind:k8s          # used for agentSelector routing in Job definitions

namespace: ci          # Kubernetes namespace where job Pods are created
maxConcurrent: 5       # maximum number of concurrent Pods

# Fallback image when no podTemplate is specified
podImage: golang:1.24-alpine

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
          # command omitted → agent auto-injects "sleep infinity"

  node:
    workspace:
      mountPath: /workspace
    spec:
      containers:
        - name: job
          image: node:20-alpine
```

### 2. Starting the agent

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
    - kind:k8s
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
    - kind:k8s
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
          image: aquasec/trivy:latest   # agent auto-injects "sleep infinity"
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

1. Create the Pod (auto-inject `command: ["sleep", "infinity"]`)
2. Send each step into the Pod via the equivalent of `kubectl exec`
3. Report results and logs to the master in real time
4. After all steps complete, delete the Pod (or return to pool if `reuse: true`)

Use `container:` to switch the execution container per step. When omitted, the first container (`job`) is used.

---

## Artifacts and Cache

The k8s-agent supports `uploadArtifact`, `downloadArtifact`, and `cache` steps via an auto-injected sidecar container that talks **directly to S3** — object bytes never transit the controller.

### How it works

When a job pod is created, the agent automatically adds a sidecar container named `unified-artifact` to the pod, running the `unified-sidecar` binary (a static, distroless image — no shell, `tar`, or `curl` inside it). The container is kept alive with `unified-sidecar idle`; individual transfers are dispatched into it via `exec` with an explicit argv (e.g. `unified-sidecar artifact upload --run <id> --name <name> --path <dir>`), never through a shell string. The sidecar shares the pod's workspace volume and reads/writes objects in the S3-compatible bucket configured for the agent.

Object key layout is unchanged from the controller-relay model:

- Artifacts: `artifacts/{runID}/{name}.tar.gz`
- Cache: `caches/<sha>.tar.zst` (+ `caches/<sha>.meta` for TTL/metadata)

Job-container steps (`run:` commands) are unaffected — the sidecar runs in its own container and is invisible to the main step execution.

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
