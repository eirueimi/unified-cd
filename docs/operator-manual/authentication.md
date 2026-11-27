# Authentication Guide (SSO / non-SSO)

This document covers the authentication methods for the unified-cd controller API.
Both the minimal non-SSO setup and the Dex-based OIDC SSO setup are described.

> **Authorization:** see [Authorization](authorization.md) for the permission catalogue, the three role bundles, OIDC group/role mapping, per-folder grants (`oidc.permissions`) and the PAT rule.

> The CLI is referred to as `unified-cd` throughout. This is the built binary (`./bin/unified-cd`);
> source is under `cmd/unified-cli`. During development you can also use `go run ./cmd/unified-cli ...`.

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

`ServerAuth` has no name-based branch for static tokens; break-glass admin comes only from matching the configured token's hash. **`UNIFIED_TOKEN` is automatically synced to the DB
as a PAT named `env:UNIFIED_TOKEN` at startup and is treated as one of the method-1 PATs.**
This means it appears in `token list` / `token delete` like any other PAT (for a caller who may manage every token).
Its break-glass status requires the presented token's hash to match the configured `UNIFIED_TOKEN` value (constant-time) **and** the PAT to have no owner; the name alone grants nothing.
The `env:` name prefix is reserved and cannot be used for new PATs, and at startup any other PAT
named `env:UNIFIED_TOKEN` (different hash, e.g. a stale row after rotation) is removed with a warning in the controller log.
See [Authorization: Break-glass](authorization.md#break-glass) for the reserved-name error, the startup purge warning and the warning logged when `UNIFIED_TOKEN` stays set alongside SSO.
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
# Expires after tokens.defaultLifetime (2160h) unless set: --expires-in 720h
```

PATs are stored as SHA-256 hashes in the DB and managed via `token list` / `token delete`.
Each PAT created through the API records who created it (`created_by`, shown as `createdBy` by `token list`):
`break-glass`, `pat:<id>` or `oidc:<sub>`, the same ids the audit log uses as `actorId`. The `token.create`
audit row also carries the new PAT's id in `details.patId`, so a `pat:<id>` actor in later rows can be traced to
its creation even after the PAT is deleted (see [Audit log](audit.md#actor-identity)).
`token list` also shows `env:UNIFIED_TOKEN`, the PAT derived from `UNIFIED_TOKEN`, to a caller
who holds `token.manage` (as noted above, deleting it is ineffective while the environment variable is still set).
A token created with `UNIFIED_TOKEN` has no owner: it is role-only, `--role` defaults to `admin`
and `--permission` is not available (see [PAT owners and ceilings](#pat-owners-and-ceilings)).

> **Production note**: The default value `dev-token-change-me` in the repo-root `docker-compose.yaml`
> is committed in plaintext. The controller refuses to start with that value unless
> `UNIFIED_DEV_MODE=1` or `UNIFIED_ALLOW_DEFAULT_TOKEN=1` is set (the dev compose sets both).
> Use a long random `UNIFIED_TOKEN` (`openssl rand -hex 32`) for any non-local deployment;
> the production compose in `deployments/docker/` has no default and requires you to set it.

## PAT owners and ceilings

A PAT created by an SSO user (a browser session or a CLI device-flow login) records its **owner**:
that user's `sub`, `email` and the group claim values (`oidc.rolesClaim`) they presented. At use,
the PAT's permissions are recomputed on every request as *the owner's permissions under the
current config, intersected with the PAT's optional ceiling*:

- **Ceiling.** `--role <role>` bounds the token to that bundle; `--permission <name>` (repeatable)
  bounds it to a list. With neither, the PAT carries everything its owner has. A ceiling above what
  the creator holds is refused with a `400`, never silently weakened (for example
  `cannot grant role "developer": it exceeds your global role "viewer"; use --permission to grant folder permissions you hold`).
- **A PAT never exceeds its owner**, globally or on any folder. Removing the owner from `roleMap` /
  `userMap` / `permissions` takes effect for all their PATs at the next controller start, with no re-issue.
- **Group changes at the IdP** reach a PAT when its owner next logs in through the browser: every
  browser login refreshes the group snapshot on all PATs of that `sub`, even when the login itself is refused.
  Silent session refresh does not refresh PAT owners.
- **If OIDC is removed from the config**, every owner PAT fails closed with
  `forbidden: token owner has no permissions`. Role-only PATs and the break-glass `UNIFIED_TOKEN` keep working.
- **Role-only PATs** have no owner: every PAT created before this feature, and any PAT created with
  the break-glass token or another role-only PAT. They are evaluated as before, as the global bundle
  of their stored role, and they bypass folder grants until revoked; re-issue them when you introduce folders.
- A PAT created from an identity whose groups are unknown is refused with `403` (a session from
  before the upgrade, or a CLI login whose id_token carries no roles claim while the config has
  group-keyed grants). The exact strings and fixes are in
  [Troubleshooting](../troubleshooting/controller-and-database.md#token-creation-is-refused-cannot-create-a-token).
- Each user lists and revokes only their own PATs (`token.create`); `token.manage` (admin) sees and revokes everyone's.

## Agent authentication and enrollment

Agents are not human API clients. Every new VM, cloud VM or Kubernetes agent has a
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
- VMs on AWS, GCP and Azure can exchange a cloud identity proof (a signed
  STS `GetCallerIdentity` request, a Google instance identity token or an
  Azure managed-identity token) against a controller policy. They get an
  in-memory access credential only, like Kubernetes agents. Each provider was
  verified only against simulated responses in this release. See [Cloud VM
  enrollment](agents.md#cloud-vm-enrollment-aws-gcp-azure).
- A provisioning script that mints VM enrollment credentials should use an
  [enrollment-only PAT](authorization.md#enrollment-only-pats) (`agent.enroll`)
  rather than `agent.manage`.
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

A session stores the caller's `rolesClaim` values (groups) captured at login, not a fixed role.
Permissions are recomputed from those stored claims and the **current** config on every request,
so editing `roleMap` / `userMap` / `permissions` affects live sessions after a controller restart
without a re-login. The stored claims are updated on silent refresh when the refreshed id_token
carries the roles claim. Group changes at the IdP therefore appear at the next login or refresh.
A session created before the upgrade has no stored claims; it is evaluated as role-only from its
stored role and cannot create PATs until the user logs in again (at most 24 hours).

The Web UI reads the caller's effective permissions from `GET /api/v1/auth/me`, which answers
for any authenticated principal (PAT, OIDC bearer or session) and returns `name`, `kind`, `role`
(empty for a user admitted only by folder grants), `permissions` (`global` list and a `folders` map
containing only folders where a grant matched the caller) and, when known, `sub` and `email`.

### CLI login (device flow)

```bash
unified-cli login --server http://localhost:8080
```

1. CLI fetches issuer, `deviceClientId`, and endpoints from `/api/v1/auth/oidc-config` and the scopes to request (`openid`, `email`, `profile` plus the bare `oidc.rolesClaim`, so the IdP emits groups, unless `oidc.scopes` overrides the list; an older CLI requests only the first three)
2. Starts device authorization and displays the URL to open in a browser
3. Authenticate with `admin@example.com` / `password` and approve
4. CLI receives the **id_token (JWT)** and saves it to `~/.config/unified-cd/config.yaml`
5. Subsequent `apply` etc. authenticate using the stored id_token (method 2)

```bash
# After login, no token flag needed
unified-cli apply -f examples/hello.yaml --server http://localhost:8080
```

#### Token lifetime and automatic refresh

The saved id_token expires on the IdP's schedule (Dex default about 24 hours; Google's is expected to be about 1 hour, which is not verified). When the IdP also issued a **refresh token**, `login` saves it too (config fields `refreshToken`, `tokenExpiry`, `loginClient`, `loginRelay`; the file stays mode `0600`), and the CLI refreshes the id_token by itself:

- Before every command that talks to the controller, the CLI checks `tokenExpiry`. If the token expires within 60 seconds, it refreshes first and runs the command with the new token. The expiry is the `exp` claim of the id_token.
- Concurrent commands take an exclusive lock file next to the config (`<config>.lock`), re-read the config and use the token another process already refreshed. A refresh gives up after 15 seconds.
- A device-flow login against a controller **without** `deviceClientSecret` refreshes directly at the IdP's token endpoint as a public client. Logins that went through the relay (`deviceClientSecret` set) and `--browser` logins refresh through the controller (`POST /api/v1/auth/cli/refresh`).
- Refresh is **skipped** when the token comes from `--token` or `UNIFIED_TOKEN`, and when the effective server (`--server` / `UNIFIED_SERVER`) differs from the `server` stored in the config file. The stored refresh token belongs to the stored server and is never sent anywhere else.
- If a refresh fails, the CLI keeps the old token, prints `token refresh failed: <reason>; run "unified-cli login" again` to stderr once, and still runs the command. The server's `401` is the final word.
- If the IdP issued no refresh token, `login` prints `the identity provider issued no refresh token; you will need to log in again when the token expires` and the behavior is the old one: run `login` again after expiry. See [Identity providers](identity-providers.md) for the settings that are expected to make each IdP issue one (`offline_access`, Google `authParams`); the vendor behavior is [unverified](identity-providers.md#unverified-idp-behavior).

The controller keeps no refresh tokens. It only relays them, and it does not re-check admission on refresh: each request is still checked against the claims of the id_token it carries.

### CLI login with a device client secret (relay)

Some IdPs (Google's "TVs and limited-input devices" client) require a client secret for the device flow. The CLI must not hold it, so set `oidc.deviceClientSecret` (or `UNIFIED_OIDC_DEVICE_CLIENT_SECRET`) on the controller. It requires `deviceClientId`; the controller refuses to start without it (`oidc.deviceClientSecret requires oidc.deviceClientId`). Then:

- `GET /api/v1/auth/oidc-config` reports `deviceRelay: true` (and `revocationEndpoint` when the issuer has one). It never contains the secret.
- `unified-cli login` runs the device flow through the controller's relay endpoints, which add the secret and talk only to the configured issuer's own endpoints. They pass the IdP's OAuth error codes (`authorization_pending`, `slow_down`, `expired_token`, `invalid_grant`) through unchanged.

| Endpoint | Purpose |
|---|---|
| `POST /api/v1/auth/cli/device/start` | Start the device authorization |
| `POST /api/v1/auth/cli/device/token` | Poll once for the token (`{deviceCode}`) |
| `POST /api/v1/auth/cli/refresh` | Refresh an id_token (`{refreshToken, client}`, `client` is `device` or `browser`); 404 when OIDC is off or the named client has no secret configured (for example `browser` without `clientSecret`) |
| `POST /api/v1/auth/cli/revoke` | Revoke a refresh token (`{refreshToken, client}`); 404 when the issuer has no revocation endpoint, OIDC is off, or the named client has no secret configured |
| `GET /api/v1/auth/cli/browser/start` | Browser login: validate the loopback redirect and PKCE challenge and show the confirmation page (see below) |
| `POST /api/v1/auth/cli/browser/continue` | Browser login: the confirmation form; sends the browser to the IdP (form field `state`, requires the `ucd_cli_login` cookie, sets the `ucd_cli_cb` cookie; see the cookie names below) |
| `POST /api/v1/auth/cli/browser/exchange` | Redeem a browser-login one-time code (`{code, codeVerifier}`) |

These endpoints are unauthenticated and rate limited **per client IP**, separately for each endpoint (device token polling: burst 50, one request per 100 ms; refresh: burst 50, one per 200 ms; device start, revoke, browser continue and browser exchange: burst 20, one per second), sized so that about 50 users behind one address can log in at once. A rate-limited call gets `429` with `rate_limited`. Behind a reverse proxy or load balancer, set `trustedProxies` (see [High availability](high-availability.md) and the [trusted proxies migration](migrations/2026-11-09-trusted-proxies.md)); otherwise every request is attributed to the proxy and all users share one bucket. The relay never logs tokens, device codes or refresh tokens.

An old CLI against such a controller keeps working when no secret is set (the direct device flow is unchanged); a new CLI against an old controller falls back to the direct flow because the new `oidc-config` fields are absent. Once `deviceClientSecret` is set, an old CLI can no longer log in: it cannot authenticate as a confidential client and talks to the IdP directly, so it must be upgraded. A new CLI refuses `--issuer` / `--client-id` against such a controller (`--issuer and --client-id cannot be used when the server relays the device flow`).

A login that used `--issuer` or `--client-id` stores no refresh token (and no `loginClient`), because refresh and revoke through the controller would act as the controller's client rather than the one you named. Such a token simply expires; log in again.

### CLI login with the browser (`--browser`)

```bash
unified-cli login --browser --server https://unified-cd.example.com
```

This uses the controller's existing browser SSO and the existing web client, so no extra IdP client is needed. It is offered when the controller has `clientSecret` set (`browserSSOEnabled: true` in `oidc-config`) and it requires the controller key manager (otherwise `/api/v1/auth/cli/browser/start` answers 503 `browser login requires the controller key manager`).

1. The CLI listens on `127.0.0.1:<random port>` and opens `GET /api/v1/auth/cli/browser/start?redirect=http://127.0.0.1:<port>/callback&challenge=<PKCE S256>` in your browser (the URL is also printed).
2. The controller accepts only a loopback `redirect` (`http://127.0.0.1:<port>/callback` or `http://[::1]:<port>/callback`) and a base64url S256 challenge. It does **not** go to the IdP yet: it shows a **confirmation page** ("Sign in unified-cli on this computer (port N)? Only continue if you just ran `unified-cli login --browser` in a terminal on this computer.") with a Continue button and a Cancel link, and sets a short-lived `ucd_cli_login` cookie (random nonce, `HttpOnly`, `SameSite=Strict`, `Secure` unless `--insecure-cookies`, valid 10 minutes, scoped to `/api/v1/auth/cli/browser/`). When cookies are `Secure` the cookie is named `__Host-ucd_cli_login` and has `Path=/` and no `Domain` instead; see [Cookie names and the host you open](#cookie-names-and-the-host-you-open).
3. Continue posts to `/api/v1/auth/cli/browser/continue`, which sends the browser through the normal IdP login only if the cookie of that same browser matches the login attempt. Otherwise it answers 403 `confirmation required: open the link printed by unified-cli login --browser on this computer`. When it accepts, it also sets a second short-lived cookie, `ucd_cli_cb` (random nonce, `HttpOnly`, `SameSite=Lax` because the IdP-to-controller hop is a cross-site top-level navigation, `Secure` unless `--insecure-cookies`, valid 10 minutes, scoped to `/api/v1/auth/oidc-callback`; `__Host-ucd_cli_cb` with `Path=/` when `Secure`); only its SHA-256 is stored with the login attempt.
4. The IdP callback (`/api/v1/auth/oidc-callback`) first requires the `ucd_cli_cb` cookie of that same browser. Without it, or with the cookie of another attempt, it answers 403 `confirmation required: ...`, does not exchange the IdP code, issues no one-time code and does not redirect to the loopback (not even with an error). With it, the same admission check as the web login runs, without creating a session. The controller stores the tokens encrypted under a **one-time code** (stored hashed, valid for 2 minutes) and redirects the browser to the loopback address with `?code=...`. If the user is refused (no role) or the IdP returns an error, the browser is redirected to the loopback with `?error=access_denied` (or the IdP's lowercase error code) instead, and the CLI stops with `browser login was refused: <code>`.
5. The CLI redeems the code at `/api/v1/auth/cli/browser/exchange` together with the PKCE verifier. The code is single use: any attempt, including one with a wrong verifier, consumes it.

The confirmation step exists because a loopback redirect cannot prove who is listening on the port (RFC 8252 section 8.6). Without it, anyone who can bind a local port could send you a start link on the real controller domain; with an existing IdP session the login would finish silently and your code would go to their port. The confirmation page alone is not enough, because an attacker can script the start and continue requests with their own cookies and give the resulting IdP link to you. That is why the callback cookie exists: the one-time code is only issued to the browser that went through the confirmation, so an IdP link opened in a different browser is refused with 403 and nothing is sent to anyone's loopback port. The remaining precondition is that someone who can also drive your own browser through the confirmation page (for example by tricking you into pressing Continue) is not stopped by this; only press Continue when you just ran `login --browser` yourself.

#### Cookie names and the host you open

| Cookies | `ucd_cli_login` | `ucd_cli_cb` |
|---|---|---|
| `Secure` (default) | `__Host-ucd_cli_login`, `Path=/` | `__Host-ucd_cli_cb`, `Path=/` |
| `--insecure-cookies` | `ucd_cli_login`, `Path=/api/v1/auth/cli/browser/` | `ucd_cli_cb`, `Path=/api/v1/auth/oidc-callback` |

The `__Host-` prefix makes a browser refuse the cookie unless it is `Secure`, has `Path=/` and carries no `Domain`. A sibling subdomain, or a plain-http page on the same host, therefore cannot plant a cookie that the controller would accept. The prefix requires `Secure`, so a deployment on plain HTTP with `--insecure-cookies` keeps the plain names and narrow paths. Both cookies are only ever read by the CLI login endpoints; the web login ignores them. With the `__Host-` prefix they have `Path=/`, so the browser sends them with every request to the same host for up to 10 minutes; this matters if a proxy or load balancer logs `Cookie` headers.

Because the cookies are host-only, the browser must open the start link on the same host that the IdP later returns to: the host of `oidc.externalUrl` (`UNIFIED_OIDC_EXTERNAL_URL`), which is the host in the redirect URI. Without `externalUrl`, the callback host is the host you opened, so the two cannot differ. Run `unified-cli login --browser --server <url>` with a `--server` whose host matches `externalUrl`, for example `https://unified-cd.example.com` and not a second hostname, an IP address or an internal service name that reaches the same controller. Otherwise the confirmation cookie is set for one host and the IdP callback arrives at another, and the login ends with 403 `confirmation required: ...` (see [Troubleshooting](../troubleshooting/controller-and-database.md)).

The controller reuses the IdP discovery document for the CLI relay and browser-login endpoints for 10 minutes and never caches a failed discovery, so a changed IdP configuration reaches these endpoints within 10 minutes, or immediately after a controller restart. The web login always discovers anew.

The CLI waits 5 minutes for the browser. `--browser` cannot be combined with `--issuer` or `--client-id`. The controller accepts id_tokens issued to either `deviceClientId` or `clientId` as bearer tokens, so browser-login tokens work for API calls. A controller older than this feature answers 401 or 404 to the start URL; the CLI then prints `this server does not support browser login for the CLI; upgrade the controller or use the device flow`.

`oidc.authParams` adds extra parameters to this login's authorization request (and to the web login's), for example `access_type: offline` for Google.

### CLI logout

```bash
unified-cli logout
```

Revokes the stored refresh token at the IdP when the server advertises a `revocation_endpoint` (directly for a direct device login, through `/api/v1/auth/cli/revoke` for relayed and browser logins), then clears `token`, `refreshToken`, `tokenExpiry`, `loginClient` and `loginRelay` from the config. `server` and extra `headers` are kept. If revocation fails the CLI prints `warning: could not revoke the refresh token: <reason>` and still clears the local tokens. If a refresh token was stored but there is no revocation path (an older controller, or an IdP without RFC 7009 revocation) it prints `warning: the identity provider has no revocation endpoint; the refresh token stays valid until it expires`. After a PAT login, `logout` only clears the token.

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

For IdPs other than Dex (Auth0, Keycloak, Okta, etc.), see [Identity provider setup](identity-providers.md) for per-IdP registration steps, group-claim configuration and example `oidc:` blocks. In short:

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

The token obtained via `unified-cli login` is written to the config file (the path given by `--config`, if any), so after login
you only need `--server` (or `server` in the config file) without specifying a token. `login` keeps any existing `headers` in that file. Automatic refresh applies only to a token that comes from the config file, and only when the effective server matches the one stored there.

---

## Troubleshooting

| Symptom | Cause and fix |
|---------|---------------|
| `server: unauthorized` (apply etc.) | Token not set or expired. For non-SSO: check `UNIFIED_TOKEN`. For SSO: re-run `unified-cli login`. id_tokens expire on the IdP's schedule (Dex ~24h) and are refreshed automatically only when the IdP issued a refresh token; see [CLI login](#cli-login-device-flow) and [Troubleshooting: CLI login](../troubleshooting/controller-and-database.md#cli-login-and-token-refresh-problems). |
| `OIDC provider discovery error: HTTP 404` | Controller is old or OIDC is not configured. Verify SSO compose is running and `/api/v1/auth/oidc-config` returns JSON. |
| `Unregistered redirect_uri ("/device/callback")` | Dex is running with old config. Run `docker compose ... restart dex`. Check that `/device/callback` is registered in `dex-config.sso.yaml`. |
| `invalid_client / Invalid client credentials` (device callback) | CLI is using the confidential client. Verify `UNIFIED_OIDC_DEVICE_CLIENT_ID=unified-cd-cli` (public, no secret) is set and that client exists in Dex. |
| `404` on device authorization | `/dex/*` proxy is not active. Check `UNIFIED_OIDC_ISSUER_INTERNAL` is set (a WARN log is emitted at startup if it is missing). |
| `403 forbidden: no role assigned` after SSO login, or `403 forbidden: no role` on a bearer token | Nothing in `roleMap` / `userMap` / `defaultRole` matched the caller **and** no `oidc.permissions` entry matched. See [Authorization: How a caller's permissions are resolved](authorization.md#how-a-callers-permissions-are-resolved) and run `unified-cli authz explain`. |
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
