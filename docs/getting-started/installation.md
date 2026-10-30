# Installation

## Docker (recommended for production)

```bash
# Controller
docker pull ghcr.io/eirueimi/unified-cd-controller:latest

# Kubernetes agent
docker pull ghcr.io/eirueimi/unified-cd-k8s-agent:latest
```

Images are published to [GitHub Container Registry](https://github.com/eirueimi/unified-cd/pkgs/container/unified-cd-controller) on every `v*` tag for `linux/amd64` and `linux/arm64`.

A ready-to-run stack using these published images (controller + PostgreSQL + Garage + a Docker agent) lives at [`deployments/docker/docker-compose.yaml`](https://github.com/eirueimi/unified-cd/blob/main/deployments/docker/docker-compose.yaml). Unlike the repo-root `docker-compose.yaml` (source build with hot reload, for development), this one pulls the release images:

```bash
cp .env.example .env    # set UNIFIED_TOKEN
# Generate a persistent secret-encryption key (or skip this and keep the
# default UNIFIED_DEV_MODE=1 for a throwaway key — secrets won't survive a restart):
unified-cli keygen --out ./kek
# In .env set UNIFIED_CONTROLLER_KEY_FILE=/run/secrets/kek, then add this volume
# mount under the controller service in deployments/docker/docker-compose.yaml:
#   volumes:
#     - ./kek:/run/secrets/kek:ro
docker compose --env-file .env -f deployments/docker/docker-compose.yaml up -d
```

Pin a release by setting `UNIFIED_CD_VERSION` (e.g. `v0.0.3`) in `.env`. The default is `latest`; the controller image for both `latest` and `v0.11.0` was verified to start from this Compose file with PostgreSQL and Garage and report `healthy` (the health check uses the BusyBox `wget` in the Alpine-based image).

The repository-root Compose files are development-only and do not define a
production security boundary. Production controllers require HTTPS. Per-agent
credential enrollment is implemented; mTLS agent certificates are future work,
not a current deployment feature.

## Kubernetes

Install with the Helm chart. It is published to GitHub Container Registry as an
OCI artifact at `oci://ghcr.io/eirueimi/charts/unified-cd`, starting with the
first release after v0.11.0. Helm 3 and Kubernetes 1.25 or newer are required.
The chart's [README](https://github.com/eirueimi/unified-cd/blob/main/charts/unified-cd/README.md)
is the full values reference. To install from a checkout of this repository,
use `charts/unified-cd` in place of the OCI reference in every command below.

### Trial

```bash
helm install unified-cd oci://ghcr.io/eirueimi/charts/unified-cd \
  --version <x.y.z> -n unified-cd --create-namespace

kubectl -n unified-cd port-forward svc/unified-cd-controller 8080:80
```

Open <http://localhost:8080> and sign in with the token `dev-token-change-me`.

The defaults install the controller, two Kubernetes agents, the `ci` job
namespace, and an in-cluster PostgreSQL and Garage (S3) with fixed development
credentials. **This is for evaluation only; do not use it in production.** The
chart refuses to render an Ingress while those credentials are in use. The
controller may restart a few times while PostgreSQL and Garage start, which is
expected. Artifacts work out of the box: the agent uses
`sidecarS3SecretMode: broker`, so the artifact sidecar obtains S3 credentials
from the controller's [store-credential broker](../operator-manual/kubernetes-integration.md#s3-credential-delivery-mode-sidecars3secretmode).

The secret-encryption key (KEK) is generated once, in the cluster, by a Job
(`unified-cd-kek-init`). It is never replaced by a re-install or
`helm upgrade`, and it survives `helm uninstall`.

### Production

Production needs your own PostgreSQL and S3-compatible object store, two
Secrets, and HTTPS in front of the controller (the controller serves plain HTTP
on port 8080 and does not terminate TLS).

**1. Create the controller Secrets.** The chart does not create credentials in
this mode, and a missing Secret shows up as a Pod that fails to start (the
chart does not use `lookup`).

```bash
kubectl create namespace unified-cd

kubectl create secret generic unified-cd-controller -n unified-cd \
  --from-literal=UNIFIED_DB_DSN='postgres://...' \
  --from-literal=UNIFIED_TOKEN='...' \
  --from-literal=UNIFIED_S3_ENDPOINT='...' \
  --from-literal=UNIFIED_S3_BUCKET='...' \
  --from-literal=UNIFIED_S3_KEY='...' \
  --from-literal=UNIFIED_S3_SECRET='...'

unified-cli keygen --out ./kek
kubectl create secret generic unified-cd-controller-kek -n unified-cd \
  --from-file=kek=./kek
```

- `UNIFIED_DB_DSN` is the PostgreSQL connection string; `UNIFIED_TOKEN` is the
  administrator static token for human and CLI authentication.
- Omitting the four `UNIFIED_S3_*` keys does not stop the controller. It starts
  and logs `no object store configured — log archival disabled`; log archival
  and artifacts are then off.
- The KEK uses `--from-file` on purpose: `keygen --out` already writes a file,
  and `--from-literal` would put the key into `argv` and the shell history.
  `--from-env-file` is the equivalent for the other keys.
- Create the KEK once and never replace it with a different value: every secret
  encrypted under the old key becomes permanently unreadable. Back `./kek` up
  somewhere durable, then delete the local file.
- The KEK lives in its own Secret, not as one more key of
  `unified-cd-controller`. The controller projects every key of that Secret into
  its process environment (`envFrom`), which would expose the encryption key
  through `docker inspect`, `/proc/<pid>/environ`, crash dumps and child
  processes such as the git subprocess used for `AppSource` resolution. The KEK
  is instead mounted as a read-only file (`defaultMode: 0400`). Do not merge the
  two Secrets. Use a different Secret name with `controller.kek.secretName`.

**2. Install**, pointing at your Secret and an Ingress with TLS:

```bash
helm install unified-cd oci://ghcr.io/eirueimi/charts/unified-cd \
  --version <x.y.z> -n unified-cd \
  --set bundledStores.enabled=false \
  --set controller.existingSecret=unified-cd-controller \
  --set ingress.enabled=true \
  --set ingress.className=nginx \
  --set 'ingress.hosts[0].host=cd.example.com' \
  --set 'ingress.hosts[0].paths[0].path=/' \
  --set 'ingress.hosts[0].paths[0].pathType=Prefix' \
  --set 'ingress.tls[0].secretName=cd-tls' \
  --set 'ingress.tls[0].hosts[0]=cd.example.com' \
  --set agent.server=https://cd.example.com
```

`agent.server`, when set, must be an `https://` URL. The Ingress does not
create certificates; for cert-manager, add its annotation through
`ingress.annotations` (for example
`cert-manager.io/cluster-issuer: letsencrypt`). If another load balancer or
service mesh gateway terminates TLS instead, leave `ingress.enabled=false` and
set `agent.server` to its URL. Leaving `agent.server` empty makes the chart set
`allowInsecureHTTP: true` and point the agent at the controller over plain
in-cluster HTTP. That happens whenever the controller is installed, not only in
trial mode, so a production install with `bundledStores.enabled=false` and no
`agent.server` gets the insecure setting too (the install NOTES print a
warning). Always set `agent.server` in production.

Put non-secret controller and agent settings in `controller.config` and
`agent.config`; they are deep-merged over the chart's defaults, and changing
them restarts the Pods.

**SSO / OIDC (optional).** Add OIDC keys to the `unified-cd-controller` Secret.
Without them the controller uses only the static `UNIFIED_TOKEN`; with them
`UNIFIED_TOKEN` stays as an administrator and CLI fallback. Only
`UNIFIED_OIDC_ISSUER` and `UNIFIED_OIDC_CLIENT_ID` are required to enable SSO.

| Key | Required | Description |
|---|---|---|
| `UNIFIED_OIDC_ISSUER` | Yes | Public OIDC issuer URL, for example `https://accounts.google.com`. |
| `UNIFIED_OIDC_CLIENT_ID` | Yes | OIDC client ID registered with your identity provider. |
| `UNIFIED_OIDC_CLIENT_SECRET` | For browser SSO | Client secret for the Authorization Code Flow. Omit only for public clients. |
| `UNIFIED_OIDC_DEVICE_CLIENT_ID` | For CLI login | Client ID of the public client used by the CLI device flow. |
| `UNIFIED_OIDC_ISSUER_INTERNAL` | For an in-cluster IdP | Internal URL the controller uses to reach the IdP (for example `http://dex.unified-cd.svc.cluster.local:5556/dex`). Also enables the `/dex/*` reverse proxy so the browser reaches an in-cluster Dex through the controller. |
| `UNIFIED_OIDC_EXTERNAL_URL` | Rarely | Override for the redirect URI base, when the controller's `Host` header differs from the URL the browser uses. |

Set the redirect URI at your IdP to `https://<your-domain>/api/v1/auth/oidc-callback`.
With an external IdP, add only the public-facing keys. For an in-cluster Dex,
run Dex as its own Deployment in the release namespace and point the
controller at it. The controller reverse-proxies `/dex/*` to Dex, so the browser
never needs to reach Dex directly. Set these keys in the
`unified-cd-controller` Secret alongside the others:

```bash
UNIFIED_OIDC_ISSUER='https://<your-domain>/dex'
UNIFIED_OIDC_ISSUER_INTERNAL='http://dex.unified-cd.svc.cluster.local:5556/dex'
UNIFIED_OIDC_CLIENT_ID='unified-cd'
UNIFIED_OIDC_CLIENT_SECRET='your-client-secret'
UNIFIED_OIDC_DEVICE_CLIENT_ID='unified-cd-cli'
```

A minimal Dex `ConfigMap` for this setup:

```yaml
issuer: https://<your-domain>/dex

storage:
  type: memory   # use a persistent backend (postgres, etcd) for production

web:
  http: 0.0.0.0:5556

oauth2:
  skipApprovalScreen: true

staticClients:
  - id: unified-cd
    secret: your-client-secret
    name: unified-cd
    redirectURIs:
      - https://<your-domain>/api/v1/auth/oidc-callback

  - id: unified-cd-cli
    public: true
    name: unified-cd CLI
    redirectURIs:
      - /device/callback

connectors:
  # connect to your upstream IdP here, or use enablePasswordDB for testing
```

> **Persistent storage.** `storage: type: memory` loses Dex's sessions and
> refresh tokens whenever its Pod restarts, which signs everyone out. Use a
> persistent backend (PostgreSQL or Kubernetes CRDs) in production.

The static clients are the same two described in
[Authentication](../operator-manual/authentication.md#sso-setup-oidc-dex);
`docker-compose.sso.yml` and `dex-config.sso.yaml` in the repository root are a
working local example.

**Vault / OpenBao instead of a KEK file (optional).** To wrap the key with
Transit, put `UNIFIED_KMS_URI`, `UNIFIED_VAULT_ADDR`,
`UNIFIED_VAULT_AUTH=kubernetes` and `UNIFIED_VAULT_AUTH_PARAM=role=unified-cd`
in the `unified-cd-controller` Secret (the controller's ServiceAccount is
`unified-cd-controller`; bind the Vault role to it and the release namespace).
The controller refuses to start with both a key file and a KMS URI, but the
chart always sets `UNIFIED_CONTROLLER_KEY_FILE` and mounts the KEK Secret.
Until the chart has a switch for this, remove the `UNIFIED_CONTROLLER_KEY_FILE`
environment entry and the `controller-kek` volume and volume mount from the
controller Deployment, for example with a Helm post-renderer. See the
[Secrets Management Guide](../user-guide/secrets.md#using-vault-or-openbao-transit)
for the Transit key and policy, and the
[High Availability Guide](../operator-manual/high-availability.md#vault-openbao-when-unified_kms_uri-is-used).

Static-token auth (`UNIFIED_VAULT_AUTH=token` with `UNIFIED_VAULT_TOKEN_FILE`)
also works on Kubernetes, but it gives up the credential-free advantage of the
`kubernetes` method, where the controller's projected ServiceAccount token is the
credential Vault verifies and no token Secret is mounted. Prefer it only when
Vault policy requires a specific token per deployment.

### Agent only

To connect a Kubernetes agent to a controller running elsewhere (for example
Docker Compose on a host), install only the agent and the job namespace:

```bash
helm install unified-cd-agent oci://ghcr.io/eirueimi/charts/unified-cd \
  --version <x.y.z> -n unified-cd --create-namespace \
  --set controller.enabled=false \
  --set bundledStores.enabled=false \
  --set agent.server=https://controller.example.com
```

First configure the external controller with an `agentAuth.kubernetesClusters`
entry for this cluster and an enabled enrollment policy that binds the agent's
ServiceAccount (`unified-cd-k8s-agent`) and namespace; see
[Kubernetes Integration](../operator-manual/kubernetes-integration.md). Do not
substitute a static token or create an agent credential Secret.

### Release bundles (`kubectl apply`)

Each GitHub Release also attaches three pre-rendered bundles for clusters
without Helm. They are rendered from the same chart. Bundles are published as
release assets starting with **v0.6.0**.

```bash
# Trial: controller + k8s-agent + PostgreSQL + Garage (development credentials)
# Pinned to a release
kubectl apply -f https://github.com/eirueimi/unified-cd/releases/download/v0.11.0/install.yaml
# Or always the newest release
kubectl apply -f https://github.com/eirueimi/unified-cd/releases/latest/download/install.yaml
```

| File | Contents | Prerequisites |
|------|----------|---------------|
| `install.yaml` | controller + k8s-agent + in-cluster PostgreSQL and Garage | None. Evaluation only. |
| `core-install.yaml` | controller + k8s-agent | External PostgreSQL and S3 store, a TLS terminator, and the Secrets from the Production section. Download it and replace `https://controller.example.invalid` with your HTTPS endpoint. |
| `agent-only.yaml` | k8s-agent only | A controller running elsewhere with the matching enrollment policy. Download it and replace `https://controller.example.invalid` with its HTTPS endpoint. |

The bundles run the KEK Job as a plain Job (`kubectl apply` has no hooks);
re-applying never replaces an existing KEK. Re-applying a newer `install.yaml` over a v0.11.0 install works in place and keeps the KEK; afterwards delete the orphaned `garage-config-<hash>` ConfigMap (see the [migration guide](../operator-manual/migrations/2026-10-30-helm-chart.md)). For production, prefer
`helm install`: the bundles are fixed renderings, so changing anything means
editing the downloaded file. Coming from the old Kustomize overlays? See
[Migrating from the Kustomize overlays to Helm](../operator-manual/migrations/2026-10-30-helm-chart.md).

## Binaries

Pre-built binaries for Linux, macOS, and Windows (amd64/arm64) are available on the [Releases page](https://github.com/eirueimi/unified-cd/releases):

```bash
# Example: Linux amd64, pinned to the latest tagged release. Archive names
# are versioned (goreleaser's name_template), so update the version below to
# match whichever release you're installing.
curl -L https://github.com/eirueimi/unified-cd/releases/download/v0.5.0/unified-cd_0.5.0_linux_amd64.tar.gz | tar xz
sudo mv unified-cli /usr/local/bin/
```

Or install from source with Go:

```bash
go install github.com/eirueimi/unified-cd/cmd/unified-cd-agent@latest    # → $GOBIN/unified-cd-agent
go install github.com/eirueimi/unified-cd/cmd/controller@latest  # controller
go install github.com/eirueimi/unified-cd/cmd/unified-cli@latest # CLI
```
