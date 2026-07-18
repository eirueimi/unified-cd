# Authorization (RBAC)

unified-cd has three roles — **admin**, **developer**, **viewer** — resolved at authentication time and enforced per API route. Roles are strictly hierarchical: `viewer < developer < admin`.

## Permission matrix

| Resource / action | viewer | developer | admin |
|---|:--:|:--:|:--:|
| Jobs — list/get/yaml | ✅ | ✅ | ✅ |
| Jobs — create/update/delete | ❌ | ✅ | ✅ |
| Runs — list/get/logs/events | ✅ | ✅ | ✅ |
| Runs — trigger / cancel | ❌ | ✅ | ✅ |
| Approvals — approve/reject | ❌ | ✅ | ✅ |
| Schedules — list/get | ✅ | ✅ | ✅ |
| Schedules — create/delete | ❌ | ✅ | ✅ |
| Secrets — list names | ❌ | ✅ | ✅ |
| Secrets — set/delete | ❌ | ❌ | ✅ |
| AppSources / WebhookReceivers — list/get | ✅ | ✅ | ✅ |
| AppSources / WebhookReceivers — create/delete | ❌ | ❌ | ✅ |
| GitCredentials — all (incl. list) | ❌ | ❌ | ✅ |
| PAT — create (≤ own role) | ❌ | ✅ | ✅ |
| Agents — info GET | ✅ | ✅ | ✅ |
| Agent enrollments / identities / enrollment policies | viewer reads; admin creates, changes, revokes, or disables | viewer/admin | viewer/admin |
| Agents — register/heartbeat/claim | per-agent principal (not a human role) | | |

Secret *values* are never returned by the API (names only). Artifact download requires an authenticated caller (agent token or any human role).

## How roles are resolved

Each caller gets a role:

- **PAT**: stored per-token (`role` column). Created via the API, clamped to ≤ the creator's role. The bootstrap `UNIFIED_TOKEN` PAT is always **admin** (break-glass).
- **OIDC (SSO login / id_token)**: resolved from token claims via configuration (below).
- **Agent principal**: separate from human roles. New agents authenticate with
  a short-lived per-agent access credential; controller-issued identity labels
  and capabilities are authoritative. The legacy shared token, when explicitly
  configured, is temporary compatibility only.

## Agent authorization boundaries

A non-legacy credential is bound to one immutable `agentId`. The controller
rejects a differing route/body ID with `agent identity mismatch` (403), and
preserves the run's immutable `claimed_by` check for writes. An agent cannot
grant itself scheduling labels or capabilities. Administrators manage
one-time VM enrollments, Kubernetes enrollment policies, and identity enable,
disable, and credential revocation through the agent lifecycle API/CLI.

**Legacy shared-token agents are guarded by the same per-run ownership
check, not exempted from it.** Every write to a run — step reports, log
lines, step/run outputs, sidecar status, finishing the run, and artifact
uploads — is checked against that run's `claimed_by`, whether the caller
presents a per-agent credential or the legacy shared token. A write to a run
the caller did not claim under the agent ID it presents gets `403 run <id>
is claimed by another agent`. The artifact-upload route has no per-agent
path segment for a legacy caller to bind an identity to, so a legacy shared
token can never satisfy that check there: **every legacy artifact upload is
unconditionally rejected with 403** (which fails the step and the run,
absent `continueOnError`), regardless of which run it targets. A fleet with
legacy agents that upload artifacts must migrate those agents to per-agent
enrollment credentials before running jobs with `uploadArtifact:` steps.
Secret fetch is fenced the same way for every principal: an agent can only
request secret names the run's own spec declares, or it gets `403 secret not
needed by this run` (see [Secrets: Access
control](secrets.md#access-control)).

See [Migration: agent authentication](migration-agent-auth.md) and
[Migration: security hardening](migration-2026-07-security-hardening.md) for
the rollout and the legacy-mode retirement check.

### OIDC role resolution config

Add to the controller `oidc:` config:

```yaml
oidc:
  issuer:      https://...
  clientId:    unified-cd
  clientSecret: ...
  rolesClaim:  groups                 # ID-token claim to read role values from (default "groups")
  roleMap:                            # claim value -> role
    unified-admins:  admin
    unified-devs:    developer
    unified-viewers: viewer
  userMap:                            # email or sub -> role (individual override; IdPs without groups)
    alice@example.com:      admin
    breakglass@example.com: admin
  defaultRole: viewer                 # role when nothing matches; "" or "deny" => login denied
```

Scalars `rolesClaim` and `defaultRole` can also come from `UNIFIED_OIDC_ROLES_CLAIM` / `UNIFIED_OIDC_DEFAULT_ROLE`. **`roleMap`/`userMap` are maps and can only be set in the config file** (there is no env var for them).

Break-glass is not part of this resolution — it is a separate PAT-only
short-circuit in `ServerAuth` (`internal/controller/auth.go`), checked before
OIDC role resolution ever runs (see [Break-glass](#break-glass) below).
**`resolveRole`'s resolution order** (`internal/controller/rbac.go`), for an
OIDC-authenticated caller: `userMap` (email, then sub) → `roleMap`
(highest-rank match wins) → `defaultRole` (`""`/`"deny"` ⇒ 403).

> `groups` is NOT a standard OIDC claim — whether it exists and what it contains is entirely up to the IdP. `rolesClaim` lets you point at whatever claim your IdP emits (`groups`, `roles`, a namespaced claim, or even `email`).

## SSO topologies

unified-cd is a generic OIDC relying party and points at exactly **one** issuer. Both setups below use the same `roleMap`/`userMap`/enforcement — only `issuer`/`clientId`/`rolesClaim` and the presence of Dex connectors differ.

### (A) Dex as broker (recommended for multiple sources / GitHub / uniform CLI device flow)

unified-cd → Dex; add a Dex connector per identity source.

```yaml
# unified-cd
oidc:
  issuer:         https://<controller>/dex
  clientId:       unified-cd
  clientSecret:   unified-cd-secret
  deviceClientId: unified-cd-cli
  rolesClaim:     groups
  roleMap:
    "my-org:platform": admin       # value shape depends on the connector
    "mygroup/devs":    developer
  userMap:
    alice@example.com: admin
  defaultRole:    viewer
```

```yaml
# Dex connectors (add only what you need)
connectors:
  - type: github            # groups: "org:team" — GitHub is non-OIDC, so this connector is REQUIRED for GitHub
    id: github
    config: { clientID: ..., clientSecret: ..., redirectURI: https://<dex>/callback, orgs: [{name: my-org}], teamNameField: slug }
  - type: gitlab            # groups: "group/subgroup"
    id: gitlab
    config: { baseURL: https://gitlab.example.com, clientID: ..., clientSecret: ... }
  - type: oidc              # Entra / Auth0 / generic — passes upstream claims through
    id: entra
    config: { issuer: https://login.microsoftonline.com/<tenant>/v2.0, clientID: ..., clientSecret: ..., insecureEnableGroups: true, scopes: [openid, profile, email] }
# local static users (no groups → use userMap by email):
# enablePasswordDB: true
# staticPasswords: [{ email: alice@example.com, hash: "...", username: alice }]
```

Groups are fetched at login by the single connector the user authenticates through and baked into the id_token; unified-cd reads the claim from the token (it does not query Dex per request). Group changes take effect on next login / token refresh.

### (B) Direct to an IdP (no Dex)

Point `oidc.issuer` straight at the IdP. `roleMap`/enforcement are identical to (A).

```yaml
# Entra ID (App Roles — recommended; emits a "roles" claim)
oidc:
  issuer:       https://login.microsoftonline.com/<tenant-id>/v2.0
  clientId:     <app-client-id>
  clientSecret: <client-secret>
  rolesClaim:   roles
  roleMap: { admin: admin, developer: developer, viewer: viewer }
  defaultRole:  deny
# For the CLI device flow, enable "Allow public client flows" on the app registration.
```

```yaml
# GitLab (self-hosted)
oidc:
  issuer:       https://gitlab.example.com          # exposes /.well-known/openid-configuration
  clientId:     <application-id>
  clientSecret: <secret>
  rolesClaim:   groups_direct                        # GitLab's group claim name (verify for your version)
  roleMap: { "mygroup/admins": admin, "mygroup/devs": developer }
  defaultRole:  viewer
# CLI device flow support depends on GitLab version; use topology (A) if you rely on it.
```

```yaml
# Auth0 (namespaced custom claim via a Post-Login Action)
oidc:
  issuer:       https://<tenant>.us.auth0.com/
  clientId:     <client-id>
  clientSecret: <secret>
  rolesClaim:   "https://unified-cd.example.com/roles"
  roleMap: { admin: admin, developer: developer, viewer: viewer }
  defaultRole:  deny
```

> **GitHub cannot be used directly** — it is not an OIDC provider (no id_token; org/team membership is only available via its REST API). Use topology (A) with the Dex `github` connector.

### Who produces the group values

| IdP (setup) | who builds the group/role claim | rolesClaim | roleMap value shape |
|---|---|---|---|
| local Dex (static) | (no groups) | `email` | use `userMap` (email→role) |
| Dex → GitHub | the Dex `github` connector (calls the GitHub API) | `groups` | `org:team` |
| Dex → GitLab | GitLab (or Dex `gitlab` connector) | `groups` | group path |
| Entra ID | Entra itself (App Roles or groups claim) | `roles` / `groups` | app-role value / group GUID |
| Auth0 | Auth0 (Post-Login Action) | namespaced claim | Auth0 role name |

## Break-glass

The static `UNIFIED_TOKEN` PAT is always **admin**. Treat it as emergency-only: store it securely, do not use it for daily work, rotate it periodically. It is the way back in if the IdP is unavailable or misconfigured.

## Migration / backward compatibility

- Existing PATs and sessions migrate to `role = 'admin'` (column default), so current single-token workflows keep working. Afterward, re-issue least-privilege PATs.
- The controller still boots without OIDC (bootstrap admin only). "SSO required" is an operational policy plus always-on authorization, not a hard boot gate.
- When first enabling OIDC, start with `defaultRole: viewer`, populate `roleMap`/`userMap`, then tighten to `defaultRole: deny`.
