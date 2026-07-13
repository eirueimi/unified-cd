# Configuration Reference

Complete reference for all environment variables, flags, and config files for the controller, agent, and k8s-agent.

## Table of Contents

- [Controller](#controller)
  - [Flags](#controller-flags)
  - [Environment Variables](#controller-environment-variables)
  - [Config File](#controller-config-file)
- [Agent](#agent)
  - [Flags](#agent-flags)
  - [Environment Variables](#agent-environment-variables)
  - [Config File](#agent-config-file)
- [K8s Agent](#k8s-agent)
  - [Flags](#k8s-agent-flags)
  - [Config File](#k8s-agent-config-file)
- [Priority Order](#priority-order)

---

## Controller

### Controller Flags

```
unified-cd-controller [FLAGS]

  -f                      string   Config file path (YAML)
  --dsn                   string   PostgreSQL DSN (env: UNIFIED_DB_DSN)
  --addr                  string   Listen address (default: :8080)
  --token                 string   Static bearer token (env: UNIFIED_TOKEN)
  --s3-endpoint           string   S3-compatible endpoint (env: UNIFIED_S3_ENDPOINT)
  --s3-bucket             string   S3 bucket name (env: UNIFIED_S3_BUCKET)
  --s3-key                string   S3 access key ID (env: UNIFIED_S3_KEY)
  --s3-secret             string   S3 secret access key (env: UNIFIED_S3_SECRET)
  --data-dir              string   Local object store directory (env: UNIFIED_DATA_DIR)
  --web-dir               string   Static web assets directory (env: UNIFIED_WEB_DIR)
  --ui-proxy-target       string   Vite dev server URL to proxy /ui/* (env: UNIFIED_UI_PROXY_TARGET)
  --log-level             string   Log level: debug, info, warn, error (env: UNIFIED_LOG_LEVEL)
  --log-stderr-plain      bool     Render run-log stderr the same color as stdout instead of red (env: UNIFIED_LOG_STDERR_PLAIN)
  --audit-retention-days  int      Days to keep audit_logs rows; 0 = keep forever (default: 90, env: UNIFIED_AUDIT_RETENTION_DAYS)
  --insecure-cookies      bool     Do not set the Secure attribute on session cookies (env: UNIFIED_INSECURE_COOKIES)
```

### Controller Environment Variables

| Variable | Required | Description |
|---|---|---|
| `UNIFIED_DB_DSN` | **Yes** | PostgreSQL connection string, e.g. `postgres://user:pass@host:5432/db?sslmode=disable` |
| `UNIFIED_TOKEN` | Yes (without SSO) | Static admin bearer token. Auto-synced to DB as a PAT named `env:UNIFIED_TOKEN`. Required when OIDC is not configured. |
| `UNIFIED_CONTROLLER_KEY` | Recommended | 32-byte hex master key for secret encryption (`openssl rand -hex 32`). If unset, auto-generated and persisted to DB. **All replicas must share the same value in HA setups.** |
| `UNIFIED_LOG_LEVEL` | No | Log level: `debug`, `info` (default), `warn`, `error` |
| `UNIFIED_S3_ENDPOINT` | No | S3-compatible object store endpoint (e.g. `garage.internal:3900`). Without S3, log archival and artifacts are disabled. |
| `UNIFIED_S3_BUCKET` | No | S3 bucket name |
| `UNIFIED_S3_KEY` | No | S3 access key ID |
| `UNIFIED_S3_SECRET` | No | S3 secret access key |
| `UNIFIED_DATA_DIR` | No | Local directory used as object store fallback (alternative to S3, development only) |
| `UNIFIED_WEB_DIR` | No | Path to static web assets directory. If unset, `/ui/*` returns 404. |
| `UNIFIED_UI_PROXY_TARGET` | No | Proxy `/ui/*` to a Vite dev server (e.g. `http://localhost:5173`). Used during frontend development. |
| `UNIFIED_LOG_STDERR_PLAIN` | No | When `true`, the web UI renders run-log stderr the same color as stdout instead of red. Default: red. |
| `UNIFIED_OIDC_ISSUER` | No (SSO only) | OIDC issuer URL, e.g. `https://accounts.example.com` or `http://localhost:8080/dex` |
| `UNIFIED_OIDC_ISSUER_INTERNAL` | No (SSO only) | Internal issuer URL for Docker/container scenarios where the public issuer URL is not reachable inside the container |
| `UNIFIED_OIDC_EXTERNAL_URL` | No (SSO only) | External base URL for OIDC callbacks, e.g. `https://unified-cd.example.com`. Required when the controller is behind a reverse proxy with a different external URL. |
| `UNIFIED_INSECURE_COOKIES` | No | When `true`, omits the `Secure` attribute from session cookies. Only for plain-HTTP deployments (see [Authentication](authentication.md)); leave unset (default `false`) whenever TLS is terminated anywhere in front of the controller. |
| `UNIFIED_OIDC_CLIENT_ID` | No (SSO only) | OIDC client ID for browser SSO (confidential client) |
| `UNIFIED_OIDC_CLIENT_SECRET` | No (SSO only) | OIDC client secret |
| `UNIFIED_OIDC_DEVICE_CLIENT_ID` | No (SSO only) | OIDC public client ID for CLI device flow. Falls back to `UNIFIED_OIDC_CLIENT_ID` if unset. |
| `UNIFIED_AUDIT_RETENTION_DAYS` | No | Days to keep `audit_logs` rows before a leader-only background task deletes them. Default `90`. `0` = keep forever. See [docs/audit.md](audit.md). |

> **Reverse proxy note**: the controller rejects cross-origin state-changing requests by comparing the
> browser's `Origin` header host against the request's `Host` header. If you put a reverse proxy in
> front of the controller, it **must forward the original `Host` header** (e.g. nginx:
> `proxy_set_header Host $host;`), or every POST/PUT/DELETE from the Web UI will be rejected with 403.
> With OIDC enabled, `UNIFIED_OIDC_EXTERNAL_URL` (`externalUrl` in the config file) additionally
> whitelists the public host for this same check.

### Controller Config File

All flags can be specified in a YAML config file (`-f config.yaml`):

```yaml
dsn: postgres://unified:unified@localhost:5432/unified?sslmode=disable
addr: :8080
token: dev-secret
controllerKey: a1b2c3d4...      # 32-byte hex (openssl rand -hex 32)

s3Endpoint: garage.internal:3900
s3Bucket: unified-cd
s3Key: garageadmin
s3Secret: garageadmin12345
dataDir: /var/lib/unified-cd    # local object-store fallback (dev; alternative to S3)

webDir: /srv/unified-cd/web         # static web assets; unset => /ui/* returns 404
uiProxyTarget: http://localhost:5173 # dev only: proxy /ui/* to a Vite dev server
stderrPlain: false                   # true => run-log stderr is the same color as stdout
insecureCookies: false                # true => drop Secure attribute on session cookies (plain-HTTP deployments only)

# Log level is a flag/env, not a config-file field: --log-level / UNIFIED_LOG_LEVEL

# SSO (OIDC) is a nested block (not flat oidc* keys):
oidc:
  issuer: http://localhost:8080/dex
  issuerInternal: http://dex:5556/dex   # in-container discovery URL
  externalUrl: http://localhost:8080    # browser redirect base (behind a proxy)
  clientId: unified-cd
  clientSecret: unified-cd-secret        # set => browser SSO enabled
  deviceClientId: unified-cd-cli         # public client for the CLI device flow
  rolesClaim: groups
  defaultRole: viewer
  roleMap:                               # OIDC group/role -> unified-cd role
    unified-admins: admin
  userMap:                               # email/subject -> unified-cd role
    alice@example.com: admin
```

Config-file keys map 1:1 to the flags above (e.g. `dataDir` ↔ `--data-dir`), with OIDC settings under the nested `oidc:` block. See `internal/config/controller.go` for the authoritative field list.

---

## Agent

### Agent Flags

```
unified-cd-agent [FLAGS]

  -f                      string    Config file path (default: unified-agent.yaml if exists)
  --server                string    Controller URL (env: UNIFIED_AGENT_SERVER)
  --token                 string    Agent bearer token (env: UNIFIED_AGENT_TOKEN)
  --id                    string    Agent identifier (default: hostname; env: UNIFIED_AGENT_ID)
  --labels                string    Comma-separated labels, e.g. "kind:linux,env:prod" (env: UNIFIED_AGENT_LABELS)
  --expose-env            string    Comma-separated env vars to expose to job steps (env: UNIFIED_AGENT_EXPOSE_ENV)
  --cache-endpoint        string    S3/MinIO endpoint for cache storage
  --cache-key             string    Cache storage access key ID
  --cache-secret          string    Cache storage secret access key
  --cache-bucket          string    Cache storage bucket name
  --max-concurrent        int       Max simultaneous runs (default: 1)
  --clean-workspace       bool      Wipe workspace before each run
  --workspace-dir         string    Base directory for run workspaces (default: ~/workspace; env: UNIFIED_AGENT_WORKSPACE_DIR)
  --drain-timeout         duration  Max wait after SIGTERM before forced shutdown (0 = wait forever)
  --pause-image           string    Image for the claim pod's pause (netns-holder) container (default: busybox:1.36)
  --runner-image          string    Default primary container image for isolated jobs without a podTemplate job container (default: ghcr.io/eirueimi/unified-cd-runner:v0.0.3)
  --log-level             string    Log level: debug, info, warn, error (env: UNIFIED_AGENT_LOG_LEVEL)
```

`--pause-image` and `--runner-image` configure the standard agent's [claim
pod](agents.md#job-isolation-on-the-standard-agent-claim-pod), built for
every isolated (non-`native`) job claim. They have no dedicated environment
variable — set them via flag or the `pauseImage`/`runnerImage` config-file
keys below.

### Agent Environment Variables

| Variable | Description |
|---|---|
| `UNIFIED_AGENT_SERVER` | Controller URL |
| `UNIFIED_AGENT_TOKEN` | Agent bearer token (must match controller's `UNIFIED_TOKEN` or be a valid PAT) |
| `UNIFIED_AGENT_ID` | Agent identifier (defaults to hostname if not set) |
| `UNIFIED_AGENT_LABELS` | Comma-separated labels, e.g. `kind:docker,env:prod` |
| `UNIFIED_AGENT_EXPOSE_ENV` | Comma-separated host environment variable names to pass through to job steps |
| `UNIFIED_AGENT_WORKSPACE_DIR` | Base directory for run workspaces (default: `~/workspace`) |
| `UNIFIED_AGENT_LOG_LEVEL` | Log level: `debug`, `info` (default), `warn`, `error` |
| `UNIFIED_CACHE_ENDPOINT` | S3/MinIO endpoint for cache storage (env equivalent of `--cache-endpoint`) |
| `UNIFIED_CACHE_KEY` | Cache storage access key ID (env equivalent of `--cache-key`) |
| `UNIFIED_CACHE_SECRET` | Cache storage secret access key (env equivalent of `--cache-secret`) |
| `UNIFIED_CACHE_BUCKET` | Cache storage bucket name (env equivalent of `--cache-bucket`) |

Additionally, every step receives `UNIFIED_AGENT_OS` (`linux`, `darwin`, or `windows`) automatically.

### Agent Config File

```yaml
# unified-agent.yaml
server: http://unified-cd-controller:8080
token: my-agent-token
id: worker-01
labels:
  - kind:linux
  - env:prod
exposeEnv:
  - HOME
  - PATH
  - GOPATH

cacheEndpoint: garage.internal:3900
cacheKey: garageadmin
cacheSecret: garageadmin12345
cacheBucket: unified-cd-cache

maxConcurrent: 4
cleanWorkspace: false
workspaceDir: /data/unified-cd/workspace
drainTimeout: 60s
pauseImage: busybox:1.36                              # claim pod pause container (default shown)
runnerImage: ghcr.io/eirueimi/unified-cd-runner:v0.0.3 # default primary container (default shown)
logLevel: info
```

Start with config file:

```bash
./bin/unified-cd-agent -f unified-agent.yaml
```

### Job isolation notes

Every job is isolated by default (see [Job Isolation: `native` and the claim
pod](jobs.md#job-isolation-native-and-the-claim-pod)); the standard agent
needs a container runtime (docker, podman, or nerdctl) to run isolated jobs.

- **Rootless podman is the recommended runtime on Linux hosts.** It avoids
  root-owned files leaking into the bind-mounted workspace (the container's
  root maps to the agent's own user), sidestepping the EPERM-fallback
  cleanup path entirely — see [Agent Labels and Routing: Workspace
  lifecycle](agents.md#workspace-lifecycle).
- **On macOS/Windows, `--workspace-dir`/`workspaceDir` must live under a
  path your container runtime's file sharing exposes** — e.g. under
  `/Users` for Docker Desktop on macOS. A workspace root outside the
  runtime's shared paths fails to bind-mount into the claim pod.
- Apple's `container` CLI is **not auto-detected** and is not supported for
  isolated jobs (its runtime can't join another container's network
  namespace). Select it explicitly with `--container-runtime container` only
  for non-isolated `runsIn.image` steps; use docker or podman for isolated
  jobs.

---

## K8s Agent

### K8s Agent Flags

```
unified-cd-k8s-agent [FLAGS]

  --config       string   Config file path (env: UNIFIED_K8S_CONFIG)
  --secret       string   Secret override file path, merged over the config (env: UNIFIED_K8S_SECRET)
  --log-level    string   Log level: debug, info, warn, error (env: UNIFIED_K8S_LOG_LEVEL)
```

All agent settings live in the config file (`--config` / `UNIFIED_K8S_CONFIG`); sensitive values may be split into a Secret file (`--secret` / `UNIFIED_K8S_SECRET`) merged on top.

### K8s Agent Config File

```yaml
# k8s-agent-config.yaml
server: http://unified-cd-controller:8080
token: my-agent-token
agentId: k8s-agent-1
labels:
  - kind:k8s
  - cluster:prod

namespace: ci               # Kubernetes namespace for job Pods
maxConcurrent: 10           # max simultaneous Pods

# Fallback image when no podTemplate is referenced. Bash-less/sh-less images
# (as here) work fine by default — steps exec via the injected ucd-sh shim,
# not a shell that has to exist in the image (see the podImage guidance
# under "K8s Agent config fields" below).
podImage: alpine:3.19

# Image the prepended init container runs to install the ucd-sh shim onto
# the shared /.ucd volume. Defaults to the k8s-agent's own image (below);
# override for air-gapped registries that mirror it under another name.
# shimImage: ghcr.io/eirueimi/unified-cd-k8s-agent:latest

# kubeconfig: /path/to/kubeconfig
# Omit to use InClusterConfig (when running inside the cluster)
# or ~/.kube/config (when running outside)

# Artifact-transfer sidecar injected into every job/scope Pod
# (default: ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest)
sidecarImage: ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest

# Name of a Secret carrying UNIFIED_S3_* env for the sidecar. Enables
# cache/artifact transfers; when unset, cache steps are no-ops (reported
# Succeeded) and artifact upload/download steps fail.
sidecarS3SecretName: unified-cd-s3

# How long an idle pooled job Pod is kept for reuse before teardown
# (Go duration, e.g. "10m"). Unset = no reuse window.
poolIdleTimeout: 10m

# Log level is a flag/env, not a config-file field: --log-level / UNIFIED_K8S_LOG_LEVEL

# Named pod templates referenced in Job YAML via podTemplate.name
podTemplates:

  golang:
    workspace:
      mountPath: /workspace
    spec:
      containers:
        - name: job
          image: golang:1.24-alpine
          # command omitted → agent injects ["/.ucd/ucd-sh", "pause"]

  node:
    workspace:
      mountPath: /workspace
      pvc:
        storageClassName: standard
        storageRequest: 5Gi
        accessMode: ReadWriteOnce
    spec:
      containers:
        - name: job
          image: node:20-alpine
        - name: playwright
          image: mcr.microsoft.com/playwright:v1.44.0-jammy

  python:
    workspace:
      mountPath: /workspace
    spec:
      containers:
        - name: job
          image: python:3.12-slim
      volumes:
        - name: pip-cache
          emptyDir: {}
```

### K8s Agent config fields

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `server` | string | Yes | — | Controller base URL the agent claims runs from |
| `token` | string | Yes | — | Agent bearer token (put in the Secret file, not the ConfigMap) |
| `agentId` | string | Yes | — | Unique agent ID. Overridden by env `UNIFIED_K8S_AGENT_ID` (e.g. the pod name) |
| `labels` | []string | No | — | Agent labels matched against a Job's `agentSelector` |
| `namespace` | string | No | `default` | Namespace the agent creates job/scope Pods in |
| `podImage` | string | No | `ghcr.io/eirueimi/unified-cd-runner:v0.0.3` | Fallback job-container image when no `podTemplate` is referenced. Bash-less/sh-less images work (`alpine`, busybox-based) — steps exec via the injected `ucd-sh` shim by default, not a shell the image must provide. Truly empty images (`scratch`, distroless-static) cannot run steps on the k8s agent: env application prepends the `env` binary, which they lack (exit 127). See [Job Reference: Shell (`shell:`)](jobs.md#shell-shell). |
| `shimImage` | string | No | `ghcr.io/eirueimi/unified-cd-k8s-agent:latest` | Image the prepended `ucd-shim` init container runs to install the `ucd-sh` shim onto the shared `/.ucd` `emptyDir`. Override for air-gapped registries mirroring the k8s-agent image under another name. |
| `sidecarImage` | string | No | `ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest` | Artifact-transfer sidecar image injected into every job/scope Pod |
| `sidecarS3SecretName` | string | No | — | Secret carrying `UNIFIED_S3_*` for the sidecar. Without it, cache steps are no-ops (Succeeded) and artifact steps fail |
| `kubeconfig` | string | No | in-cluster / `~/.kube/config` | Path to a kubeconfig; omit to use `InClusterConfig` (in cluster) or `~/.kube/config` (out of cluster) |
| `maxConcurrent` | int | No | `5` | Max simultaneous job Pods |
| `poolIdleTimeout` | string | No | `0` (no reuse) | Go duration an idle pooled Pod is kept for reuse before teardown (e.g. `10m`) |
| `podTemplates` | map | No | — | Named Pod templates referenced from Job YAML via `podTemplate.name` (see below) |

`token` (and any other sensitive value) may be placed in a separate Secret file (env `UNIFIED_K8S_SECRET`), whose fields are merged on top of the config file.

### Pod template fields

| Field | Type | Description |
|---|---|---|
| `workspace.mountPath` | string | Mount path inside the pod (default: `/workspace`) |
| `workspace.pvc.claimName` | string | Existing PVC to mount (for persistent caches) |
| `workspace.pvc.storageClassName` | string | StorageClass for ephemeral PVC creation |
| `workspace.pvc.storageRequest` | string | Storage size, e.g. `5Gi` |
| `workspace.pvc.accessMode` | string | `ReadWriteOnce`, `ReadOnlyMany`, or `ReadWriteMany` |
| `spec` | map | Kubernetes PodSpec (containers, volumes, etc.) |

The primary `job` container always receives `["/.ucd/ucd-sh", "pause"]` as
its keep-alive command (previously `["sleep", "infinity"]`), unconditionally
overriding any `command`/`args` a `podTemplate` set on it — a Go-implemented
keep-alive that needs no `sleep` binary in the image, reaps zombie children
as PID 1, and exits promptly on SIGTERM. Sidecars are left untouched: a
sidecar with its own `command`/`args` (or none at all, relying on the
image's own entrypoint — e.g. a `mysql`/`redis` service container) runs
exactly as declared; only the primary container's injection is
unconditional. Every container in the Pod also gets `/.ucd` mounted
read-only, carrying the `ucd-sh` binary installed by a prepended
`ucd-shim` init container — see [Kubernetes Integration: `/.ucd` shim
injection](kubernetes-integration.md#ucd-shim-injection).

---

## Priority Order

For all components, configuration is resolved in this order (highest priority first):

```
CLI flags  >  config file  >  environment variables
```

Examples:
- `--server http://override.example.com` overrides `UNIFIED_AGENT_SERVER`
- `UNIFIED_AGENT_SERVER=http://env.example.com` overrides the `server:` value in the config file
- The config file value is the lowest-priority fallback

---

## Minimum Production Setup

### Controller

```bash
UNIFIED_DB_DSN="postgres://user:pass@postgres-ha:5432/unified?sslmode=require" \
UNIFIED_TOKEN="$(openssl rand -hex 32)" \
UNIFIED_CONTROLLER_KEY="$(openssl rand -hex 32)" \
UNIFIED_S3_ENDPOINT="s3.amazonaws.com" \
UNIFIED_S3_BUCKET="my-unified-cd" \
UNIFIED_S3_KEY="AKIAIOSFODNN7EXAMPLE" \
UNIFIED_S3_SECRET="wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" \
./bin/unified-cd-controller --addr :8080
```

### Agent

```bash
UNIFIED_AGENT_SERVER="http://unified-cd-controller:8080" \
UNIFIED_AGENT_TOKEN="same-token-as-UNIFIED_TOKEN" \
UNIFIED_AGENT_ID="worker-$(hostname)" \
UNIFIED_AGENT_LABELS="kind:linux,env:prod" \
./bin/unified-cd-agent --max-concurrent 4
```
