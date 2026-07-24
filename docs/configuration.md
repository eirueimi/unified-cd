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
  --run-retention-days    int      Days to keep terminal (Succeeded/Failed/Cancelled) runs; 0 = keep forever (default: 0, env: UNIFIED_RUN_RETENTION_DAYS)
  --log-trim-days         int      Days after a run's logs are archived before its DB log rows are deleted; 0 = never trim (default: 0, env: UNIFIED_LOG_TRIM_DAYS)
  --insecure-cookies      bool     Do not set the Secure attribute on session cookies (env: UNIFIED_INSECURE_COOKIES)
```

### Controller Environment Variables

| Variable | Required | Description |
|---|---|---|
| `UNIFIED_DB_DSN` | **Yes** | PostgreSQL connection string, e.g. `postgres://user:pass@host:5432/db?sslmode=disable` |
| `UNIFIED_TOKEN` | Yes (without SSO) | Static admin bearer token. Auto-synced to DB as a PAT named `env:UNIFIED_TOKEN`. Required when OIDC is not configured. |
| `UNIFIED_CONTROLLER_KEY_FILE` | Required | Path to a file containing a 32-byte hex master key (`unified-cli keygen --out /etc/unified-cd/kek`). Mutually exclusive with `UNIFIED_KMS_URI`. **All replicas must be given the same file in HA setups.** |
| `UNIFIED_KMS_URI` | Optional | External KMS: `hashivault://[<mount>/]<key>` (default mount `transit`). The controller wraps DEKs with Vault/OpenBao Transit and never holds the key itself. |
| `UNIFIED_VAULT_ADDR` | With KMS | Vault/OpenBao address. |
| `UNIFIED_VAULT_AUTH` | Optional | `token` (default) or `kubernetes`. |
| `UNIFIED_VAULT_AUTH_PARAM` | With `kubernetes` | Comma-separated `key=value`; `kubernetes` requires `role`. |
| `UNIFIED_VAULT_TOKEN_FILE` | With `token` | Path to a file holding the token. Preferred over `VAULT_TOKEN`: a file does not leak into `docker inspect` or child processes, and can be replaced without a restart. |
| `VAULT_TOKEN` | With `token` | Fallback when no token file is set. |
| `UNIFIED_DEV_MODE` | Optional | `1` generates an ephemeral key. Secrets are unreadable after a restart. Never use in production. |
| `UNIFIED_GIT_RESOLVE_DEADLINE` | No | How long a run's `git://` template resolution may keep failing (network/credentials) before the run is Failed instead of waiting as Pending. Go duration, default `1h`. Deterministic resolution errors (e.g. a nonexistent ref) still fail the run immediately. There is no CLI flag; unset/invalid/non-positive values use the default. The run is failed at its *next resolution attempt* after the deadline elapses, which the per-run failure backoff may delay by up to an additional 1 hour. |
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
| `UNIFIED_RUN_RETENTION_DAYS` | No | Days to keep terminal (Succeeded/Failed/Cancelled) runs. When a run expires, its database records **and** its object-store data (archived logs, artifacts) are deleted. `0` (default) keeps runs forever. Deletion is irreversible — an expired run can no longer be replayed, because `run replay` uses the run's stored spec snapshot. |
| `UNIFIED_LOG_TRIM_DAYS` | No | Days after a run's logs are archived before its database log rows are deleted (tiered log storage). Reads (WebUI viewer, CLI, SSE) transparently switch to the archived `logs.ndjson` in the object store; the first view of a trimmed run pays one object fetch. `0` (default) never trims. Requires an object store; must be smaller than `--run-retention-days` when both are set (otherwise retention deletes runs before trimming would fire — the controller logs a warning). |

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

# Kubernetes workload enrollment for the default controller and k8s-agent manifests.
# An omitted kubeconfig uses the controller Pod's ServiceAccount identity.
agentAuth:
  kubernetesClusters:
    - name: in-cluster
  kubernetesEnrollmentPolicies:
    - name: unified-cd-k8s-agents
      cluster: in-cluster
      namespaces: [unified-cd]
      serviceAccounts: [unified-cd-k8s-agent]
      allowedLabels: [kind:kubernetes]
      requiredLabels: [kind:kubernetes]
      accessTokenTTL: 1h
      enabled: true
```

Config-file keys map 1:1 to the flags above (e.g. `dataDir` ↔ `--data-dir`), with OIDC settings under the nested `oidc:` block. See `internal/config/controller.go` for the authoritative field list.

---

## Agent

### Agent Flags

```
unified-cd-agent [FLAGS]

  -f                      string    Config file path (default: unified-agent.yaml if exists)
  --server                string    Controller URL (env: UNIFIED_SERVER)
  --credential-file       string    Protected VM refresh-credential file (default: $HOME/.unified-cd/<id>/credential.json, or $HOME/.unified-cd/credential.json when --id is unset; env: UNIFIED_AGENT_CREDENTIAL_FILE)
  --enrollment-token-file string    One-time VM enrollment-token file (env: UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE)
  --id                    string    Agent identifier (optional; adopted from the enrollment token / persisted credential when unset, asserted when set; env: UNIFIED_AGENT_ID)
  --labels                string    Comma-separated labels, e.g. "kind:linux,env:prod" (env: UNIFIED_AGENT_LABELS)
  --expose-env            string    Comma-separated env vars to allow into job steps; steps get ONLY these plus a minimal OS baseline, never the agent's full environment (env: UNIFIED_AGENT_EXPOSE_ENV)
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
  --min-free-disk         uint64    Minimum free space in bytes on the workspace filesystem required to keep claiming runs; 0 disables the check (host agent only) (env: UNIFIED_AGENT_MIN_FREE_DISK)
  --workspace-retention-days int    Age in days after which an inactive per-job workspace directory becomes eligible for removal by the opt-in workspace GC; 0 disables it (default; persistent workspaces are a feature) (host agent only) (env: UNIFIED_AGENT_WORKSPACE_RETENTION_DAYS)
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
| `UNIFIED_SERVER` | Controller URL |
| `UNIFIED_AGENT_CREDENTIAL_FILE` | Protected persistent VM refresh-credential file. Required for secure VM mode. |
| `UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE` | One-time VM enrollment credential file. Required only until initial enrollment succeeds. |
| `UNIFIED_AGENT_ID` | Agent identifier (optional; adopted from the enrollment token / persisted credential when unset, asserted when set) |
| `UNIFIED_AGENT_LABELS` | Comma-separated labels, e.g. `kind:docker,env:prod` |
| `UNIFIED_AGENT_EXPOSE_ENV` | Comma-separated host environment variable names to pass through to job steps. **This is an allowlist, not an add-on.** A native (`spec.native: true`) step no longer inherits the agent's process environment at all — it only sees a minimal OS baseline (`PATH`, `HOME`, etc.) plus whatever is named here plus the orchestrator's own step env (`env:`, secrets). A variable a job used to read implicitly must be named here explicitly, or the step sees it as unset. Agent credentials (`UNIFIED_CACHE_KEY`, `UNIFIED_CACHE_SECRET`, `UNIFIED_TOKEN`) are dropped unconditionally even if named here — there is no way to expose them to a step. |
| `UNIFIED_AGENT_WORKSPACE_DIR` | Base directory for run workspaces (default: `~/workspace`) |
| `UNIFIED_AGENT_LOG_LEVEL` | Log level: `debug`, `info` (default), `warn`, `error` |
| `UNIFIED_CACHE_ENDPOINT` | S3/MinIO endpoint for cache storage (env equivalent of `--cache-endpoint`) |
| `UNIFIED_CACHE_KEY` | Cache storage access key ID (env equivalent of `--cache-key`) |
| `UNIFIED_CACHE_SECRET` | Cache storage secret access key (env equivalent of `--cache-secret`) |
| `UNIFIED_CACHE_BUCKET` | Cache storage bucket name (env equivalent of `--cache-bucket`) |
| `UNIFIED_AGENT_MIN_FREE_DISK` | Minimum free bytes on the workspace filesystem required to keep claiming runs (env equivalent of `--min-free-disk`). `0`/unset disables the check. **Host agent only** — the k8s-agent's job workspaces are pod volumes, not host disk, so there is no preflight to run there. |
| `UNIFIED_AGENT_WORKSPACE_RETENTION_DAYS` | Age in days after which an inactive per-job workspace directory becomes eligible for removal by the opt-in workspace GC (env equivalent of `--workspace-retention-days`). `0` (default) disables the GC entirely. **Host agent only.** |

Additionally, every step receives the following environment variables automatically:
- `UNIFIED_AGENT_OS` — the agent host OS (`linux`, `darwin`, or `windows`)
- `UNIFIED_WORKSPACE` — the absolute path of the run workspace as seen inside the step (the step's working directory); user `env:` may override it.

### Agent Config File

```yaml
# unified-agent.yaml
server: https://unified-cd-controller.example.com
id: worker-01
credentialFile: /var/lib/unified-cd-agent/credentials.json
enrollmentTokenFile: /var/lib/unified-cd-agent/enrollment.token
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
minFreeDisk: 5368709120        # 5Gi; below this free space on workspaceDir's filesystem, stop claiming (0 = disabled, default)
workspaceRetentionDays: 0      # >0 opts in to the periodic per-job workspace GC (0 = disabled, default)
logLevel: info
```

VM agents authenticate only via enrollment; there is no `token` config field.
On first start, the agent consumes the one-time enrollment-token file and
writes the refresh credential to `credentialFile`; later starts rotate that
credential automatically. When `credentialFile` is left unset (no flag, env,
or config value), the agent defaults it to
`$HOME/.unified-cd/<id>/credential.json` and creates that owner-only
directory on startup, so only `enrollmentTokenFile` is strictly required on
a fresh host. Keep both files owner-readable only and do not put either
value in a command line. The controller, not this file, is authoritative for
labels (fixed at enrollment time by an administrator). Capabilities are not
configurable at all — the agent auto-detects and self-reports them on every
registration; see [Capabilities and routing](agents.md#capabilities-and-routing).

Start with config file:

```bash
./bin/unified-cd-agent -f unified-agent.yaml
```

`minFreeDisk` and `workspaceRetentionDays` are **host agent only** — the
k8s-agent's job workspaces are ephemeral pod volumes, not host disk, so
neither a disk preflight nor a directory GC applies there.

- **`minFreeDisk`** (bytes) is a pre-claim disk check on the workspace
  filesystem (`workspaceDir`). Below the threshold, the agent stops claiming
  new runs on that slot and retries after a short backoff — it never deletes
  anything and never fails the run it would have claimed. `0` (default)
  disables the check. This is an operational lever, not an error condition:
  pair it with disk-usage monitoring/alerting (see [Operations Guide:
  Monitoring Points](operations.md#monitoring-points)) so an operator notices
  *why* an agent stopped claiming.
- **`workspaceRetentionDays`** (days) opt-in-enables a periodic sweep (hourly,
  plus once at startup) that removes per-job workspace directories
  (`workspaceDir/working<slot>/<job>`) whose mtime is older than the
  retention window. `0` (default) disables the GC entirely — persistent
  per-job workspaces are a feature (they act as an inter-run cache), so
  reclaiming them must be an explicit opt-in. The sweep never removes
  `workspaceDir` itself, never removes a `working<slot>` directory itself,
  never touches a dot-prefixed sibling (e.g. the `.ucd-tools` shim directory),
  and never removes a directory belonging to a currently-active run (cross-
  checked against that agent process's live in-flight claims). See
  [Operations Guide: Workspace and Claim-Container
  Hygiene](operations.md#workspace-and-claim-container-hygiene) for when to
  enable it.

Both knobs follow the same [priority order](#priority-order) as every other
agent setting — flag > config file > environment variable — since the
resolved config-file/env value is used as the flag's own default (see
`internal/config/agent.go`'s `AgentEffective`); an explicit `--min-free-disk`
or `--workspace-retention-days` flag always wins.

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
  --log-level    string   Log level: debug, info, warn, error (env: UNIFIED_K8S_LOG_LEVEL)
```

All agent settings live in the config file (`--config` / `UNIFIED_K8S_CONFIG`). The agent authenticates via the projected ServiceAccount token mounted by the Pod (workload enrollment); it does not need a Secret file.

Kubernetes workload enrollment requires an `https://` controller URL. The controller process itself does not terminate TLS, so production deployments must provide this URL through an Ingress, load balancer, or service-mesh TLS gateway. Plain HTTP is accepted only for loopback local development, or when the configuration explicitly sets `allowInsecureHTTP: true`; the latter is reserved for intentional development-only deployments such as the bundled `install.yaml`.

### K8s Agent Config File

```yaml
# k8s-agent-config.yaml
server: https://controller.example.invalid # replace with your TLS terminator URL
enrollmentPolicy: unified-cd-k8s-agents
serviceAccountTokenFile: /var/run/secrets/unified-cd-agent/token
labels:
  - kind:kubernetes

namespace: ci               # Kubernetes namespace for job Pods
maxConcurrent: 10           # max simultaneous Pods (0/unset -> 100; negative -> unlimited)
podStartTimeout: 5m         # max wait for a run Pod to reach Running before failing the run (default shown)
drainTimeout: 0             # max wait for in-flight runs to finish on shutdown; 0 = wait forever (default shown)

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
| `server` | string | Yes | - | Controller base URL the agent claims runs from |
| `enrollmentPolicy` | string | Yes (secure mode) | - | Controller policy used to exchange the projected ServiceAccount token. The controller assigns the canonical agent ID and authorized labels. |
| `serviceAccountTokenFile` | string | No | `/var/run/secrets/unified-cd-agent/token` | Projected ServiceAccount token file. It is reread whenever the agent re-enrolls after access-token expiry. |
| `agentId` | string | No (runtime only) | - | Populated by the agent after enrollment from verified Kubernetes identity; not a config input. |
| `labels` | []string | No | — | Agent labels matched against a Job's `agentSelector` |
| `namespace` | string | No | `default` | Namespace the agent creates job/scope Pods in |
| `podImage` | string | No | `ghcr.io/eirueimi/unified-cd-runner:v0.0.3` | Fallback job-container image when no `podTemplate` is referenced. Bash-less/sh-less images work (`alpine`, busybox-based) — steps exec via the injected `ucd-sh` shim by default, not a shell the image must provide. Truly empty images (`scratch`, distroless-static) cannot run steps on the k8s agent: env application prepends the `env` binary, which they lack (exit 127). See [Job Reference: Shell (`shell:`)](jobs.md#shell-shell). |
| `shimImage` | string | No | `ghcr.io/eirueimi/unified-cd-k8s-agent:latest` | Image the prepended `ucd-shim` init container runs to install the `ucd-sh` shim onto the shared `/.ucd` `emptyDir`. Override for air-gapped registries mirroring the k8s-agent image under another name. |
| `sidecarImage` | string | No | `ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest` | Artifact-transfer sidecar image injected into every job/scope Pod |
| `sidecarS3SecretName` | string | No | — | Secret carrying `UNIFIED_S3_*` for the sidecar. Without it, cache steps are no-ops (Succeeded) and artifact steps fail |
| `kubeconfig` | string | No | in-cluster / `~/.kube/config` | Path to a kubeconfig; omit to use `InClusterConfig` (in cluster) or `~/.kube/config` (out of cluster) |
| `maxConcurrent` | int | No | `100` | Max simultaneous job Pods, enforced by a semaphore around the claim loop. `0`/unset → `100`. A **negative** value (e.g. `-1`) → unlimited: no agent-side cap, bounded only by cluster scheduling/quota. A positive value is that exact concurrency bound. |
| `podStartTimeout` | string | No | `5m` | Go duration bounding how long the agent waits for a run Pod to reach `Running` before failing the run. Env override: `UNIFIED_K8S_POD_START_TIMEOUT`. Prevents an unschedulable or `ImagePullBackOff` Pod (which under `RestartPolicy: Never` never transitions to `Failed` on its own) from wedging a run forever. Unset, unparseable, or non-positive values fall back to the default. The wait is also aborted early, without overriding the controller's status, if the run is already terminal at the controller (cancel/reap raced the Pod becoming ready). |
| `drainTimeout` | string | No | `0` (wait indefinitely) | Go duration bounding the graceful-shutdown drain window. Env override: `UNIFIED_K8S_DRAIN_TIMEOUT`. On SIGTERM/rollout the agent immediately stops claiming new runs but lets in-flight runs keep going — heartbeats keep beating during drain so a draining run isn't reaped as stuck — until either they finish or `drainTimeout` elapses, whichever comes first. `0`/unset waits forever for in-flight runs to finish (no forced cutoff). Parity with the standard agent's `--drain-timeout`. |
| `poolIdleTimeout` | string | No | `0` (no reuse) | Go duration an idle pooled Pod is kept for reuse before teardown (e.g. `10m`) |
| `podTemplates` | map | No | — | Named Pod templates referenced from Job YAML via `podTemplate.name` (see below) |

The default agent Deployment projects a ServiceAccount token with audience `unified-cd-agent-enrollment` and mounts it read-only at `/var/run/secrets/unified-cd-agent`. Do not add a token Secret to that Deployment.

`UNIFIED_K8S_POD_START_TIMEOUT` and `UNIFIED_K8S_DRAIN_TIMEOUT` override their config fields.

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
- `--server http://override.example.com` overrides `UNIFIED_SERVER`
- `UNIFIED_SERVER=http://env.example.com` overrides the `server:` value in the config file
- The config file value is the lowest-priority fallback

---

## Minimum Production Setup

### Controller

```bash
unified-cli keygen --out /etc/unified-cd/kek

UNIFIED_DB_DSN="postgres://user:pass@postgres-ha:5432/unified?sslmode=require" \
UNIFIED_TOKEN="$(openssl rand -hex 32)" \
UNIFIED_CONTROLLER_KEY_FILE="/etc/unified-cd/kek" \
UNIFIED_S3_ENDPOINT="s3.amazonaws.com" \
UNIFIED_S3_BUCKET="my-unified-cd" \
UNIFIED_S3_KEY="AKIAIOSFODNN7EXAMPLE" \
UNIFIED_S3_SECRET="wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" \
./bin/unified-cd-controller --addr :8080
```

### Agent

```bash
UNIFIED_SERVER="https://controller.example.invalid" \
UNIFIED_AGENT_ID="worker-$(hostname)" \
UNIFIED_AGENT_LABELS="kind:linux,env:prod" \
UNIFIED_AGENT_CREDENTIAL_FILE="/var/lib/unified-cd-agent/credentials.json" \
UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE="/var/lib/unified-cd-agent/enrollment.token" \
./bin/unified-cd-agent --max-concurrent 4
```

Create the private enrollment-token file with `unified-cli agent enrollment
create` before starting the VM agent. Production requires HTTPS; the
repository-root Compose files are development-only.
