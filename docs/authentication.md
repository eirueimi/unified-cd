# Authentication Guide (SSO / non-SSO)

This document covers the authentication methods for the unified-cd controller API.
Both the minimal non-SSO setup and the Dex-based OIDC SSO setup are described.

> **Authorization (roles/RBAC):** see [Authorization](authorization.md) for the 3-role model, OIDC group/role mapping, and per-role permissions.

> The CLI is referred to as `unified-cd` throughout. This is the built binary (`./bin/unified-cd`);
> source is under `cmd/unified-cli`. During development you can also use `go run ./cmd/unified-cli ...`.

## Table of Contents

- [Authentication Model Overview](#authentication-model-overview)
- [Non-SSO Setup (Static Token)](#non-sso-setup-static-token)
- [SSO Setup (OIDC / Dex)](#sso-setup-oidc--dex)
- [CLI Configuration Resolution Order](#cli-configuration-resolution-order)
- [Troubleshooting](#troubleshooting)

---

## Authentication Model Overview

The controller management API (`/api/v1/*`) is protected by the `ServerAuth` middleware,
which tries the following **3 methods** in order. The first successful method grants access.

| # | Method | Credential | Primary users |
|---|--------|-----------|---------------|
| 1 | PAT (Personal Access Token) | `Authorization: Bearer exc_...` | Long-lived scripts / CI / `UNIFIED_TOKEN` |
| 2 | OIDC id_token | `Authorization: Bearer <JWT>` | CLI after `unified-cli login` (SSO setup) |
| 3 | Session Cookie | `ucd_session` Cookie | Browser SSO login via Web UI |

> The agent API (`/api/v1/agents/*`) uses a separate per-agent credential path.
> Human PATs, OIDC tokens, and `UNIFIED_TOKEN` cannot authorize a new agent.
> Agents authenticate with an enrolled `uca_...` access token obtained via
> agent enrollment (see [Agents](agents.md)); it is not subject to SSO.

`ServerAuth` has no special branch for static tokens. **`UNIFIED_TOKEN` is automatically synced to the DB
as a PAT named `env:UNIFIED_TOKEN` at startup and is treated as one of the method-1 PATs.**
This means it appears in `token list` / `token delete` like any other PAT.
(Note: deleting it via `token delete` while `UNIFIED_TOKEN` is still set in the environment
will cause it to be re-synced on the next restart. To fully disable it, remove or change
the environment variable.)

Whether SSO is active is determined by whether OIDC configuration (`UNIFIED_OIDC_*`) is passed to the controller.

- **No OIDC config** → Only method 1 is active (`/api/v1/auth/oidc-*` returns 404). In this case
  `UNIFIED_TOKEN` is the only login method, so **the controller refuses to start if it is unset**
  (`token is required when SSO is not configured` error).
- **OIDC configured** → All three methods are active. `UNIFIED_TOKEN` is not required to start
  (the first admin login and PAT creation can be done via SSO).
- When both are configured, they are not mutually exclusive. `UNIFIED_TOKEN` (as a PAT) and SSO
  can always be used together — e.g. CI uses `UNIFIED_TOKEN` while humans use SSO.

---

## Non-SSO Setup (Static Token)

Minimal setup. All authentication uses a single shared `UNIFIED_TOKEN`
(internally it is synced as a PAT, but usage is unchanged from the caller's perspective).

### Starting the controller

```bash
cp .env.example .env          # set UNIFIED_TOKEN (defaults to dev-token-change-me)
docker compose up -d
```

The controller in `docker-compose.yaml` starts with (excerpt):

```yaml
environment:
  UNIFIED_TOKEN: ${UNIFIED_TOKEN:-dev-token-change-me}
  # UNIFIED_OIDC_* not set → SSO disabled
```

### CLI access

Pass the token via environment variable, flag, or config file.

```bash
# Environment variable
export UNIFIED_SERVER=http://localhost:8080
export UNIFIED_TOKEN=dev-token-change-me
unified-cli apply -f examples/hello.yaml

# Or via flags
unified-cli apply -f examples/hello.yaml \
  --server http://localhost:8080 --token dev-token-change-me

# Or via config file ~/.config/unified-cd/config.yaml
#   server: http://localhost:8080
#   token: dev-token-change-me
unified-cli apply -f examples/hello.yaml
```

### Web UI access

- Vite dev server: `http://localhost:5173/ui/`
- Controller directly: `http://localhost:8080/ui/`

With SSO disabled, "Login with SSO" does not work (`/api/v1/auth/oidc-login` returns 404).
Use the static token when calling protected APIs from the UI.

### Issuing PATs (optional)

If you don't want to distribute the shared token, issue individual PATs.

```bash
unified-cli token create ci-bot --server http://localhost:8080 --token <UNIFIED_TOKEN>
# => displays exc_xxxxxxxx... once. Use this as the Bearer token going forward.
# To set an expiry: --expires-in 720h
```

PATs are stored as SHA-256 hashes in the DB and managed via `token list` / `token delete`.
`token list` also shows `env:UNIFIED_TOKEN`, the PAT derived from `UNIFIED_TOKEN`
(as noted above, deleting it is ineffective while the environment variable is still set).

> **Production note**: The default value `dev-token-change-me` in `docker-compose.yaml` is
> committed in plaintext. Change it for any non-local deployment.

## Agent authentication and enrollment

Agents are not human API clients. Every new VM or Kubernetes agent has a
separate controller identity; a credential for one identity cannot make calls
as another identity or write a run claimed by another agent.

- VM enrollment credentials (`uce_`) are one-time, displayed only when
  created, and exchanged at `POST /api/v1/agents/enroll`. The agent receives a
  one-hour access credential (`uca_`) and a rotating 30-day refresh credential
  (`ucr_`) stored in its protected credential file. Access credentials cannot
  renew themselves.
- Kubernetes agents exchange a projected ServiceAccount token with audience
  `unified-cd-agent-enrollment`. TokenReview and a live bound Pod check select
  a controller policy; the agent gets an in-memory access credential only, no
  refresh credential or shared Secret.
- The controller stores only SHA-256 hashes, never plaintext credentials.
  Labels and capabilities are granted by the enrollment/policy, not trusted
  from an agent request.

Admin lifecycle endpoints are under `/api/v1/agent-enrollments`,
`/api/v1/agent-identities`, and `/api/v1/agent-enrollment-policies`.

Production deployments must use HTTPS. The repository-root Compose setup is
development-only. mTLS certificate authentication is future work and is not
provided by this release.

---

## SSO Setup (OIDC / Dex)

For local development, Dex is used as the OIDC IdP.
The controller acts as a reverse proxy for Dex, exposing **a single URL `http://localhost:8080`**.

### Architecture

```
Browser / CLI
      │  http://localhost:8080
      ▼
┌─────────────────────────────┐
│ controller (:8080)          │
│  /ui/*        → Web UI      │
│  /api/v1/*    → Admin API   │
│  /dex/*       → Dex proxy   │──► dex (:5556)  issuer=http://localhost:8080/dex
│  /device/callback → /dex/...│
└─────────────────────────────┘
```

Key points:

- **Issuer is `http://localhost:8080/dex`**. Dex uses the issuer path (`/dex`) as a prefix for all routes
  (`/dex/auth`, `/dex/token`, `/dex/device/code`, ...).
- The controller forwards `/dex/*` to Dex unchanged.
- Internal container discovery uses `UNIFIED_OIDC_ISSUER_INTERNAL=http://dex:5556/dex`,
  while the browser-visible URL remains `http://localhost:8080/dex` (via `hostRewriteTransport`).
- After device flow approval, Dex redirects to bare `/device/callback`, so the controller
  rewrites this to `/dex/device/callback` before proxying.

### Two OIDC clients

`dex-config.sso.yaml` defines two clients for different purposes:

| Client | Type | Secret | Purpose |
|--------|------|--------|---------|
| `unified-cd` | confidential | yes | Browser SSO for Web UI (Authorization Code Flow) |
| `unified-cd-cli` | public | no | CLI device flow (RFC 8628) |

> The CLI uses a separate **public client without a secret** because Dex's device flow
> unconditionally validates `client.Secret == deviceReq.ClientSecret`. Using a public client
> avoids distributing the confidential client secret through the CLI (a public endpoint).

### Starting with SSO

```bash
docker compose -f docker-compose.yaml -f docker-compose.sso.yml up -d
```

Environment variables added by `docker-compose.sso.yml`:

```yaml
environment:
  UNIFIED_OIDC_ISSUER: http://localhost:8080/dex
  UNIFIED_OIDC_ISSUER_INTERNAL: http://dex:5556/dex   # for container-internal discovery (include /dex)
  UNIFIED_OIDC_CLIENT_ID: unified-cd                  # confidential client for browser SSO
  UNIFIED_OIDC_CLIENT_SECRET: unified-cd-secret
  UNIFIED_OIDC_DEVICE_CLIENT_ID: unified-cd-cli       # public client for CLI device flow
```

> **Important**: After changing `dex-config.sso.yaml`, restart Dex explicitly.
> Dex only reads its config at startup and does not detect bind-mounted file changes automatically.
> `docker compose ... up -d` alone may not recreate the Dex container.
> ```bash
> docker compose -f docker-compose.yaml -f docker-compose.sso.yml restart dex
> ```

### Web UI login (browser SSO)

1. Open `http://localhost:8080/ui/`
2. Click "Login with SSO" → `/api/v1/auth/oidc-login` redirects to Dex
3. Authenticate on the Dex login screen (default local dev user: `admin@example.com` / `password`)
4. `/api/v1/auth/oidc-callback` issues a session and sets the `ucd_session` Cookie
5. Subsequent API calls authenticate via the Cookie (method 3)

Sessions are valid for 24 hours; they are silently refreshed via the refresh token
when less than 5 minutes remain. Logout via `POST /api/v1/auth/logout`.

### CLI login (device flow)

```bash
unified-cli login --server http://localhost:8080
```

1. CLI fetches issuer, `deviceClientId`, and endpoints from `/api/v1/auth/oidc-config`
2. Starts device authorization and displays the URL to open in a browser
3. Authenticate with `admin@example.com` / `password` and approve
4. CLI receives the **id_token (JWT)** and saves it to `~/.config/unified-cd/config.yaml`
5. Subsequent `apply` etc. authenticate using the stored id_token (method 2)

```bash
# After login, no token flag needed
unified-cli apply -f examples/hello.yaml --server http://localhost:8080
```

> id_tokens expire (Dex default ~24 hours). Run `login` again after expiry.
> Automatic refresh via refresh token is not currently supported.

### Cookies over plain HTTP (`--insecure-cookies`)

Session cookies (`ucd_session`) are `Secure` by default, so browsers will only send them back over
HTTPS. If you deploy on plain HTTP with a non-`localhost` host (`localhost` gets a browser exemption),
the browser silently drops the cookie after `oidc-callback` sets it. With SSO this manifests as an
endless login redirect loop: the callback appears to succeed, but the next request has no session, so
the UI redirects back to Dex, which redirects back to the callback, forever. If you must run such a
deployment (e.g. an internal network without TLS), pass `--insecure-cookies`
(env `UNIFIED_INSECURE_COOKIES`, config file `insecureCookies`) to drop the `Secure` attribute. Prefer
terminating TLS in front of the controller instead whenever possible.

### Production / External IdP

For IdPs other than Dex (Auth0, Keycloak, Okta, etc.):

- Set `UNIFIED_OIDC_ISSUER` to the IdP's issuer URL (no `/dex` proxy needed, `ISSUER_INTERNAL` can be empty).
- Create one confidential client for browser SSO and one public client for CLI device flow.
  Set them via `UNIFIED_OIDC_CLIENT_ID` / `UNIFIED_OIDC_CLIENT_SECRET` / `UNIFIED_OIDC_DEVICE_CLIENT_ID`.
- If the IdP has no public client support, omit `UNIFIED_OIDC_DEVICE_CLIENT_ID` and the CLI will
  fall back to `UNIFIED_OIDC_CLIENT_ID` (device flow with a confidential client depends on IdP support).

---

## CLI Configuration Resolution Order

`server` and `token` are resolved in the following priority order (higher overrides lower):

1. Config file `~/.config/unified-cd/config.yaml` (override with `--config`)
2. Environment variables `UNIFIED_SERVER` / `UNIFIED_TOKEN` (applied only when config file field is empty)
3. Flags `--server` / `--token` (highest priority)

The token obtained via `unified-cli login` is written to the config file, so after login
you only need `--server` (or `server` in the config file) without specifying a token.

---

## Troubleshooting

| Symptom | Cause and fix |
|---------|---------------|
| `server: unauthorized` (apply etc.) | Token not set or expired. For non-SSO: check `UNIFIED_TOKEN`. For SSO: re-run `unified-cli login`. id_tokens expire in ~24h. |
| `OIDC provider discovery error: HTTP 404` | Controller is old or OIDC is not configured. Verify SSO compose is running and `/api/v1/auth/oidc-config` returns JSON. |
| `Unregistered redirect_uri ("/device/callback")` | Dex is running with old config. Run `docker compose ... restart dex`. Check that `/device/callback` is registered in `dex-config.sso.yaml`. |
| `invalid_client / Invalid client credentials` (device callback) | CLI is using the confidential client. Verify `UNIFIED_OIDC_DEVICE_CLIENT_ID=unified-cd-cli` (public, no secret) is set and that client exists in Dex. |
| `404` on device authorization | `/dex/*` proxy is not active. Check `UNIFIED_OIDC_ISSUER_INTERNAL` is set (a WARN log is emitted at startup if it is missing). |
| Config changes not taking effect | Controller or Dex needs a restart. Dex does not detect file changes automatically — run `restart dex`. |

### Diagnostic commands

```bash
# Check OIDC config is published
curl -s http://localhost:8080/api/v1/auth/oidc-config | jq

# Dex discovery (via proxy)
curl -s http://localhost:8080/dex/.well-known/openid-configuration | jq

# Config loaded by Dex
docker compose -f docker-compose.yaml -f docker-compose.sso.yml exec dex cat /etc/dex/config.yaml

# Controller OIDC startup logs
docker compose -f docker-compose.yaml -f docker-compose.sso.yml logs controller | grep -i oidc
```
