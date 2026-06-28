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

  -f, --file              string   Config file path (YAML)
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
| `UNIFIED_OIDC_ISSUER` | No (SSO only) | OIDC issuer URL, e.g. `https://accounts.example.com` or `http://localhost:8080/dex` |
| `UNIFIED_OIDC_ISSUER_INTERNAL` | No (SSO only) | Internal issuer URL for Docker/container scenarios where the public issuer URL is not reachable inside the container |
| `UNIFIED_OIDC_EXTERNAL_URL` | No (SSO only) | External base URL for OIDC callbacks, e.g. `https://unified-cd.example.com`. Required when the controller is behind a reverse proxy with a different external URL. |
| `UNIFIED_OIDC_CLIENT_ID` | No (SSO only) | OIDC client ID for browser SSO (confidential client) |
| `UNIFIED_OIDC_CLIENT_SECRET` | No (SSO only) | OIDC client secret |
| `UNIFIED_OIDC_DEVICE_CLIENT_ID` | No (SSO only) | OIDC public client ID for CLI device flow. Falls back to `UNIFIED_OIDC_CLIENT_ID` if unset. |

### Controller Config File

All flags can be specified in a YAML config file (`-f config.yaml`):

```yaml
dsn: postgres://unified:unified@localhost:5432/unified?sslmode=disable
addr: :8080
token: dev-secret
controllerKey: a1b2c3d4...   # 32-byte hex

s3Endpoint: garage.internal:3900
s3Bucket: unified-cd
s3Key: garageadmin
s3Secret: garageadmin12345

logLevel: info

oidcIssuer: http://localhost:8080/dex
oidcIssuerInternal: http://dex:5556/dex
oidcClientID: unified-cd
oidcClientSecret: unified-cd-secret
oidcDeviceClientID: unified-cd-cli
```

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
  --drain-timeout         duration  Max wait after SIGTERM before forced shutdown (0 = wait forever)
  --log-level             string    Log level: debug, info, warn, error (env: UNIFIED_AGENT_LOG_LEVEL)
```

### Agent Environment Variables

| Variable | Description |
|---|---|
| `UNIFIED_AGENT_SERVER` | Controller URL |
| `UNIFIED_AGENT_TOKEN` | Agent bearer token (must match controller's `UNIFIED_TOKEN` or be a valid PAT) |
| `UNIFIED_AGENT_ID` | Agent identifier (defaults to hostname if not set) |
| `UNIFIED_AGENT_LABELS` | Comma-separated labels, e.g. `kind:docker,env:prod` |
| `UNIFIED_AGENT_EXPOSE_ENV` | Comma-separated host environment variable names to pass through to job steps |
| `UNIFIED_AGENT_LOG_LEVEL` | Log level: `debug`, `info` (default), `warn`, `error` |

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
drainTimeout: 60s
logLevel: info
```

Start with config file:

```bash
./bin/unified-cd-agent -f unified-agent.yaml
```

---

## K8s Agent

### K8s Agent Flags

```
unified-cd-k8s-agent [FLAGS]

  -f    string   Config file path (env: UNIFIED_K8S_CONFIG)
```

All configuration is through the config file or `UNIFIED_K8S_CONFIG` environment variable.

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

# Fallback image when no podTemplate is referenced
podImage: alpine:3.19

# kubeconfig: /path/to/kubeconfig
# Omit to use InClusterConfig (when running inside the cluster)
# or ~/.kube/config (when running outside)

logLevel: info

# Named pod templates referenced in Job YAML via podTemplate.name
podTemplates:

  golang:
    workspace:
      mountPath: /workspace
    spec:
      containers:
        - name: job
          image: golang:1.24-alpine
          # command omitted → agent injects ["sleep", "infinity"]

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

### Pod template fields

| Field | Type | Description |
|---|---|---|
| `workspace.mountPath` | string | Mount path inside the pod (default: `/workspace`) |
| `workspace.pvc.claimName` | string | Existing PVC to mount (for persistent caches) |
| `workspace.pvc.storageClassName` | string | StorageClass for ephemeral PVC creation |
| `workspace.pvc.storageRequest` | string | Storage size, e.g. `5Gi` |
| `workspace.pvc.accessMode` | string | `ReadWriteOnce`, `ReadOnlyMany`, or `ReadWriteMany` |
| `spec` | map | Kubernetes PodSpec (containers, volumes, etc.) |

All containers that have no `command` field automatically receive `["sleep", "infinity"]` injected by the agent.

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
