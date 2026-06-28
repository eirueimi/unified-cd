# Kubernetes Install Manifests

A complete set of manifests for installing the unified-cd `controller` and `k8s-agent` onto a Kubernetes cluster.

## Which file to use

| File | Contents | Prerequisites |
|------|----------|---------------|
| `core-install.yaml` | controller + k8s-agent only | External PostgreSQL and S3-compatible store required. Edit all `CHANGEME` placeholders before applying. |
| `install.yaml` | core-install.yaml + in-cluster PostgreSQL and Garage bundled | For evaluation / quick trial. Uses development-default credentials. **Do not use in production.** |
| `agent-only.yaml` | k8s-agent only | Controller running externally (e.g. Docker Compose). Edit `server` and `token` `CHANGEME` values before applying. |

## Applying

```bash
# Quick trial (works out of the box)
kubectl apply -f manifests/install.yaml

# Production (with external DB and S3)
# 1. Edit the CHANGEME values in manifests/core-install.yaml
# 2. kubectl apply -f manifests/core-install.yaml

# Agent only (controller running externally, e.g. Docker Compose on the host)
# 1. Edit server and token CHANGEME values in manifests/agent-only.yaml
# 2. kubectl apply -f manifests/agent-only.yaml
```

## Values to edit in core-install.yaml before applying

In the `unified-cd-controller` Secret (namespace: `unified-cd`), update the following keys:

- `UNIFIED_DB_DSN` — PostgreSQL connection string
- `UNIFIED_TOKEN` — Admin static token (must match the `token` field in the `unified-cd-k8s-agent-config` Secret)
- `UNIFIED_CONTROLLER_KEY` — 32-byte hex generated with `openssl rand -hex 32`. If left empty, the controller auto-generates and persists a key to the DB (see [HA Guide](../docs/high-availability.md))
- `UNIFIED_S3_ENDPOINT` / `UNIFIED_S3_BUCKET` / `UNIFIED_S3_KEY` / `UNIFIED_S3_SECRET` — S3-compatible object store connection info (controller starts without these, but log archival is disabled)

Also update the `token` field in the `unified-cd-k8s-agent-config` Secret (namespace: `unified-cd`) to the same value as `UNIFIED_TOKEN`.

## About install.yaml

Bundles PostgreSQL and Garage inside the cluster with the same development-default credentials as `docker-compose.yaml`
(`dev-token-change-me` / `garageadmin` / `garageadmin12345`).
Kubernetes has no equivalent of docker-compose `depends_on: condition: service_healthy`, so startup order is not guaranteed.
The `controller` will restart a few times waiting for PostgreSQL and Garage to become ready — this is expected.
Garage uses `--default-bucket` to auto-create the bucket and access key on container startup,
so no separate init Job (like the old `minio-init`) is needed.

## SSO / OIDC

SSO is optional. When not configured, the controller uses the static `UNIFIED_TOKEN` for all authentication.
When OIDC is enabled, browser login goes through the identity provider and `UNIFIED_TOKEN` is only used for agents and CLI device flow fallback.

### Environment variables

Add these to the `unified-cd-controller` Secret. Only `UNIFIED_OIDC_ISSUER` and `UNIFIED_OIDC_CLIENT_ID` are required to enable SSO; the rest depend on your setup.

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
Add only the public-facing variables to the controller Secret — no internal URL is needed:

```yaml
stringData:
  # ... existing keys ...
  UNIFIED_OIDC_ISSUER: "https://accounts.google.com"
  UNIFIED_OIDC_CLIENT_ID: "1234567890-abc.apps.googleusercontent.com"
  UNIFIED_OIDC_CLIENT_SECRET: "GOCSPX-..."
  UNIFIED_OIDC_DEVICE_CLIENT_ID: "1234567890-cli.apps.googleusercontent.com"
```

Set the redirect URI in your IDP to `https://<your-domain>/api/v1/auth/oidc-callback`.

### Option B: In-cluster Dex

Run Dex as a separate Deployment in the `unified-cd` namespace and point the controller at it.
The controller will reverse-proxy `/dex/*` to Dex so the browser never needs to reach Dex directly.

```yaml
stringData:
  # ... existing keys ...
  UNIFIED_OIDC_ISSUER: "https://<your-domain>/dex"
  UNIFIED_OIDC_ISSUER_INTERNAL: "http://dex.unified-cd.svc.cluster.local:5556/dex"
  UNIFIED_OIDC_CLIENT_ID: "unified-cd"
  UNIFIED_OIDC_CLIENT_SECRET: "your-client-secret"
  UNIFIED_OIDC_DEVICE_CLIENT_ID: "unified-cd-cli"
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

- [Kubernetes Integration Guide](../docs/kubernetes-integration.md) — k8s-agent podTemplate configuration
- [High Availability (HA) Guide](../docs/high-availability.md) — controller scale-out and leader election
