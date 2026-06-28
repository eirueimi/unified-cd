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

The only difference from a standard agent (`cmd/agent`) is where job steps run — locally vs. inside a Pod.
The communication interface with the master is identical.

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

```bash
# Inside the cluster (running as a Pod, no kubeconfig needed)
./k8s-agent --config k8s-agent-config.yaml

# Via environment variable
UNIFIED_K8S_CONFIG=k8s-agent-config.yaml ./k8s-agent
```

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
      needs: [build]
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
      needs: [build]
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
      needs: [download-deps]
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

## RBAC example

Minimum permissions required for k8s-agent to operate:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: ci
  name: unified-cd-agent
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
  name: unified-cd-agent
subjects:
  - kind: ServiceAccount
    name: unified-cd-agent
    namespace: ci
roleRef:
  kind: Role
  name: unified-cd-agent
  apiGroup: rbac.authorization.k8s.io
```
