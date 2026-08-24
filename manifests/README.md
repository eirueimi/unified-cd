# Kubernetes Install Manifests

A complete set of manifests for installing the unified-cd `controller` and `k8s-agent` onto a Kubernetes cluster.

## Which file to use

| File | Contents | Prerequisites |
|------|----------|---------------|
| `core-install.yaml` | controller + k8s-agent only | External PostgreSQL and S3-compatible store plus a TLS terminator required. Create the controller Secrets before applying, and replace the invalid k8s-agent HTTPS URL. |
| `install.yaml` | core-install.yaml + in-cluster PostgreSQL and Garage bundled | For evaluation / quick trial. Uses development-default credentials. **Do not use in production.** |
| `agent-only.yaml` | k8s-agent only | Controller running externally with the matching Kubernetes enrollment policy. Replace its example-invalid `server` URL before applying. |

## Applying

```bash
# Quick trial (development-only; the bundled manifest explicitly opts into in-cluster HTTP)
kubectl apply -f manifests/install.yaml

# Production (with external DB and S3)
# 1. Create the unified-cd-controller and unified-cd-controller-kek Secrets
#    (see "Creating the controller Secrets" below) — core-install.yaml ships
#    no Secrets of its own.
# 2. Replace `https://controller.example.invalid` in manifests/core-install.yaml
#    with the HTTPS endpoint of your TLS terminator.
# 3. kubectl apply -f manifests/core-install.yaml

# Agent only (controller running externally, e.g. Docker Compose on the host)
# 1. Configure the external controller's in-cluster verifier and enrollment policy.
# 2. Replace the example-invalid server URL in manifests/agent-only.yaml.
# 3. kubectl apply -f manifests/agent-only.yaml
```

## Creating the controller Secrets

`core-install.yaml` ships no Secrets: it references `unified-cd-controller` and
`unified-cd-controller-kek` by name (via `envFrom` and a volume mount) but does
not create them. Create both before applying `core-install.yaml`:

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

1. The KEK uses `--from-file` deliberately: `keygen --out` already writes a file,
   and `--from-literal` would put the key into `argv` and the shell history.
   `--from-env-file` is offered for the other six for the same reason.
2. Omitting the four `UNIFIED_S3_*` keys does not stop the controller. The pod
   starts and object storage is simply absent; the log says
   `no object store configured — log archival disabled`, and log archival and
   artifacts are off.
3. Fill the KEK once and never re-apply a different value — every secret
   encrypted under the old key becomes permanently unreadable.

`UNIFIED_DB_DSN` is the PostgreSQL connection string; `UNIFIED_TOKEN` is the
admin static token for human and CLI authentication. `UNIFIED_CONTROLLER_KEY_FILE`
(pointing at the `unified-cd-controller-kek` mount) is baked into
`core-install.yaml`'s controller config already — the controller refuses to
start without it (or `UNIFIED_KMS_URI`, or `UNIFIED_DEV_MODE=1`).

The default k8s-agent Deployment does not receive `UNIFIED_TOKEN` or any shared agent token. It exchanges its projected, audience-bound ServiceAccount token for a short-lived credential.

## Vault / OpenBao Kubernetes auth (optional, in place of `UNIFIED_CONTROLLER_KEY_FILE`)

To wrap the controller's key-encryption key with Vault/OpenBao Transit instead of a local
key file, set `UNIFIED_KMS_URI` and point the controller at Vault's Kubernetes auth method.
Because the controller Pod already runs under a ServiceAccount (`unified-cd-controller`),
no additional token Secret needs to be mounted — the projected ServiceAccount token doubles
as the credential Vault's Kubernetes auth method verifies. Add these keys to the
`kubectl create secret` command in place of `UNIFIED_CONTROLLER_KEY_FILE` (so the
`unified-cd-controller-kek` Secret and its volume mount are not needed at all):

```bash
kubectl create secret generic unified-cd-controller -n unified-cd \
  --from-literal=UNIFIED_DB_DSN='postgres://...' \
  --from-literal=UNIFIED_TOKEN='...' \
  --from-literal=UNIFIED_S3_ENDPOINT='...' \
  --from-literal=UNIFIED_S3_BUCKET='...' \
  --from-literal=UNIFIED_S3_KEY='...' \
  --from-literal=UNIFIED_S3_SECRET='...' \
  --from-literal=UNIFIED_KMS_URI='hashivault://unified-cd-kek' \
  --from-literal=UNIFIED_VAULT_ADDR='https://vault.example.com:8200' \
  --from-literal=UNIFIED_VAULT_AUTH='kubernetes' \
  --from-literal=UNIFIED_VAULT_AUTH_PARAM='role=unified-cd'
```

The Vault-side role must be bound to the controller's ServiceAccount and namespace. See
[Secrets Management Guide: Using Vault or OpenBao (Transit)](../docs/user-guide/secrets.md#using-vault-or-openbao-transit)
for the Transit key setup and the policy the controller needs, and
[High Availability Guide: Vault / OpenBao](../docs/operator-manual/high-availability.md#vault--openbao-when-unified_kms_uri-is-used)
for HA implications. Static-token auth (`UNIFIED_VAULT_AUTH=token` with
`UNIFIED_VAULT_TOKEN_FILE`) also works on Kubernetes but gives up the
credential-free advantage of the `kubernetes` method — prefer it only when
Vault policy requires a specific token per deployment.

The controller container itself serves HTTP on port 8080; it does not terminate TLS. Production k8s-agent configuration therefore uses an intentionally invalid HTTPS placeholder and must be changed to an externally supplied TLS endpoint (for example, an Ingress, load balancer, or service mesh gateway). The bundled `install.yaml` is the deliberate development exception: it explicitly sets `allowInsecureHTTP: true` for its in-cluster HTTP Service. Do not carry that setting into production manifests.

## Kubernetes workload enrollment

`core-install.yaml` and `install.yaml` mount a controller configuration that declares the `in-cluster` verifier and an enabled `unified-cd-k8s-agents` policy. The policy binds enrollment to ServiceAccount `unified-cd-k8s-agent` in namespace `unified-cd`, permits only the `kind:kubernetes` label and `pod`/`container` capabilities, and gives each Pod an identity derived from its verified UID.

For `agent-only.yaml`, configure the external controller equivalently before deploying the agent. The controller needs an in-cluster `agentAuth.kubernetesClusters` entry named `in-cluster`, the same policy name, and the controller ServiceAccount RBAC included in these manifests. Do not substitute a static token or create a k8s-agent credential Secret.

## About install.yaml

Bundles PostgreSQL and Garage inside the cluster with the same development-default credentials as `docker-compose.yaml`
(`dev-token-change-me` / `garageadmin` / `garageadmin12345`).
Kubernetes has no equivalent of docker-compose `depends_on: condition: service_healthy`, so startup order is not guaranteed.
The `controller` will restart a few times waiting for PostgreSQL and Garage to become ready — this is expected.
Garage uses `--default-bucket` to auto-create the bucket and access key on container startup,
so no separate init Job (like the old `minio-init`) is needed.

## SSO / OIDC

SSO is optional. When not configured, the controller uses the static `UNIFIED_TOKEN` for all authentication.
When OIDC is enabled, browser login goes through the identity provider and `UNIFIED_TOKEN` remains an administrator/CLI fallback; k8s agents use workload enrollment instead.

### Environment variables

Add these keys to the `kubectl create secret` command for `unified-cd-controller`. Only `UNIFIED_OIDC_ISSUER` and `UNIFIED_OIDC_CLIENT_ID` are required to enable SSO; the rest depend on your setup.

| Variable | Required | Description |
|---|---|---|
| `UNIFIED_OIDC_ISSUER` | Yes | Public OIDC issuer URL (e.g. `https://accounts.google.com`). Setting this (with `CLIENT_ID`) enables SSO. |
| `UNIFIED_OIDC_CLIENT_ID` | Yes | OIDC client ID registered with your identity provider. |
| `UNIFIED_OIDC_CLIENT_SECRET` | For browser SSO | Client secret for the Authorization Code Flow. Omit only for public clients. |
| `UNIFIED_OIDC_DEVICE_CLIENT_ID` | For CLI login | Client ID of the public (no-secret) client used by the CLI device flow. |
| `UNIFIED_OIDC_ISSUER_INTERNAL` | For in-cluster IDP | Internal URL the controller uses to reach the IDP for token validation and OIDC discovery (e.g. `http://dex.unified-cd.svc.cluster.local:5556/dex`). Also enables the `/dex/*` reverse proxy so the browser can reach an in-cluster Dex through the controller. |
| `UNIFIED_OIDC_EXTERNAL_URL` | Rarely needed | Override for the redirect URI base. Set this when the controller's `Host` header differs from the URL the browser uses (e.g. behind an ingress that rewrites the host). |

### Option A: External identity provider (Google, Okta, Auth0, …)

Register a web application with your IDP and obtain a client ID and secret.
Add only the public-facing keys to the `kubectl create secret` command — no internal URL is needed:

```bash
kubectl create secret generic unified-cd-controller -n unified-cd \
  --from-literal=UNIFIED_DB_DSN='postgres://...' \
  --from-literal=UNIFIED_TOKEN='...' \
  --from-literal=UNIFIED_S3_ENDPOINT='...' \
  --from-literal=UNIFIED_S3_BUCKET='...' \
  --from-literal=UNIFIED_S3_KEY='...' \
  --from-literal=UNIFIED_S3_SECRET='...' \
  --from-literal=UNIFIED_OIDC_ISSUER='https://accounts.google.com' \
  --from-literal=UNIFIED_OIDC_CLIENT_ID='1234567890-abc.apps.googleusercontent.com' \
  --from-literal=UNIFIED_OIDC_CLIENT_SECRET='GOCSPX-...' \
  --from-literal=UNIFIED_OIDC_DEVICE_CLIENT_ID='1234567890-cli.apps.googleusercontent.com'
```

Set the redirect URI in your IDP to `https://<your-domain>/api/v1/auth/oidc-callback`.

### Option B: In-cluster Dex

Run Dex as a separate Deployment in the `unified-cd` namespace and point the controller at it.
The controller will reverse-proxy `/dex/*` to Dex so the browser never needs to reach Dex directly.

```bash
kubectl create secret generic unified-cd-controller -n unified-cd \
  --from-literal=UNIFIED_DB_DSN='postgres://...' \
  --from-literal=UNIFIED_TOKEN='...' \
  --from-literal=UNIFIED_S3_ENDPOINT='...' \
  --from-literal=UNIFIED_S3_BUCKET='...' \
  --from-literal=UNIFIED_S3_KEY='...' \
  --from-literal=UNIFIED_S3_SECRET='...' \
  --from-literal=UNIFIED_OIDC_ISSUER='https://<your-domain>/dex' \
  --from-literal=UNIFIED_OIDC_ISSUER_INTERNAL='http://dex.unified-cd.svc.cluster.local:5556/dex' \
  --from-literal=UNIFIED_OIDC_CLIENT_ID='unified-cd' \
  --from-literal=UNIFIED_OIDC_CLIENT_SECRET='your-client-secret' \
  --from-literal=UNIFIED_OIDC_DEVICE_CLIENT_ID='unified-cd-cli'
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
  # connect to your upstream IDP here, or use enablePasswordDB for testing
```

See `docker-compose.sso.yml` and `dex-config.sso.yaml` in the repo root for a working local example using the same pattern.

## Regenerating manifests

Sources are in `base/` (per-component), `core-install/`, `install/`, and `agent-only/` as kustomize definitions.
Do not edit `core-install.yaml`, `install.yaml`, or `agent-only.yaml` directly — regenerate them with:

```bash
make manifests
```

## Related documentation

- [Kubernetes Integration Guide](../docs/operator-manual/kubernetes-integration.md) — k8s-agent podTemplate configuration
- [High Availability (HA) Guide](../docs/operator-manual/high-availability.md) — controller scale-out and leader election
