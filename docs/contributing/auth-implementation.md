# Authentication and authorization, in the code

The [Authentication](../operator-manual/authentication.md) and
[Authorization](../operator-manual/authorization.md) pages cover how an
operator *configures* this. This page covers how it is *built*: the two
identity systems, which middleware guards which routes, and what you must not
get wrong when adding an endpoint.

## Two identity systems, not one

This is the fact that makes the rest legible.

| | Humans and API clients | Agents |
|---|---|---|
| Type | `Principal` (`auth.go`) | `AgentPrincipal` (`agent_auth.go`) |
| Credentials | PAT (`exc_`), OIDC, browser session | enrolled access token (`uca_`) |
| Carries | Name, Kind, legacy **Role**, effective **Grants** | an agent identity — no role, no grants |
| In context under | `principalCtxKey` | its own key |

They are deliberately separate. An agent is not a low-privileged user; it is a
different kind of caller with a different question to answer ("which agent is
this, and is it allowed to act for *that* run?"), so giving it a role would be
answering the wrong question.

`Principal.Kind` is `"pat" | "oidc" | "session"`. `Principal.Role` is the
*legacy global role* (`"admin" | "developer" | "viewer"`, or `""` for a user
admitted only by `oidc.permissions` folder grants). What a human may actually
do is `Principal.Grants`, an `authz.Grants` built at authentication time: the
global role's bundle united with every matching `oidc.permissions` entry, and
for an owner PAT intersected with its ceiling.

### Permissions, not ranks

The decision logic lives in `internal/authz`, which has no dependency on the
controller:

- `catalogue.go` — the 25 permissions, which nine are folder-scopable, and the
  role bundles (`Bundle`).
- `grants.go` — `Grants`: `Can`, `Decide` (allowed plus the `grantedBy` string),
  `Explain`, `HeldAnywhere`, `Restrict` (the PAT ceiling) and `View` (the
  `/auth/me` JSON). The rule is global ∪ every proper ancestor folder.
- `policy.go` — the compiled `oidc.permissions` block and the startup
  validation strings.
- `describe.go` — the admin policy view behind `authz show`.

`roleRanks` in `rbac.go` (`viewer: 1, developer: 2, admin: 3`) now serves only
legacy role resolution and the PAT ceiling checks: an unknown or empty role
ranks **0** and is denied. `resolveRole` maps OIDC claims to a role in a fixed
order: `userMap` by email, then `userMap` by sub, then the highest-ranked match
in `roleMap`, then the default. A denial returns `("", false)` rather than a
role that happens to rank zero, so a deny is explicit rather than emergent.
`identityGrants` (`rbac.go`) is the one place that unions that role with the
matching `oidc.permissions` entries; login, `ServerAuth`, PAT owners and
`authz explain` all call it, so they cannot disagree.

## Which middleware guards what

Four middlewares matter, and picking the wrong one is the mistake to avoid.

| Middleware | Admits | Use for |
|---|---|---|
| `ServerAuth` | humans and API clients only | ordinary API routes |
| `agentAuth` | `uca_` agent credentials only | agent-only routes |
| `agentOrServerAuth` | either | routes both an agent and a human may call |
| `authorize` | looks the route up in `routeTable` and checks the human's grants | **every** human or agent-or-human route, after authentication and `auditLogMiddleware` |

`authorize` (`authz_decide.go`) is driven by `routeTable`
(`authz_routes.go`): each row is `{method, pattern, permission, scope}`, where
the scope says how the job a request acts on is found (`scopeGlobal`,
`scopeJobPath`, `scopeJobRunID`, `scopeJobQuery`, `scopeJobBody`,
`scopeScheduleBody`, `scopeSchedulePath`, `scopeFiltered`) or that the route is
`scopePublic`, `scopeSelf` or `scopeAgent`. A row may carry an `AltPerm`
(`GET` / `DELETE /api/v1/tokens`: `token.create` for your own tokens **or**
`token.manage`) and `AgentBypass: true` for agent-or-human routes, where an
agent principal skips the check and only the human branch is decided.
`agentOrServerAuth` establishes *some* principal; on its own it would let any
authenticated human in, so such a route must also be in `routeTable` with an
`AgentBypass` row, and `authorize` must be in its chain. A route that is not
in the table is refused with `403 forbidden: unclassified route`, and
`TestEveryRegisteredRouteIsClassified` fails.

For per-job routes `authorize` returns `404` when the caller cannot read the
job (indistinguishable from a missing one), `403 forbidden: missing permission
"<p>" on job "<j>"` when it can read but not act, and records the decision for
the audit log. Handlers that learn the job name only after parsing the body
(`handleApplyJob`, `handleTriggerRun`, `handleApplySchedule`) call `requireJob`
themselves; list handlers filter rows with `canReadJob`.

## Agent routes are declared in a table

`registerAgentIdentityRoutes` in `server.go` walks
`agentRouteIdentityMatrix` — a list of `{method, path, auth, bindPath,
requiredRole, handler}` — and applies the right middleware chain per entry.

This is a good pattern: the auth decision for every agent route is visible in
one place instead of scattered across registration calls. Two things follow.

**Add agent routes to the matrix, not directly to the router.** A route
registered elsewhere silently does not get `requireAgentPathIdentity`, and
nothing will tell you.

**`bindPath: true` is what stops an agent impersonating another agent.**
`requireAgentPathIdentity` rejects a request whose authenticated agent does not
match the `{agentId}` in the path. Every route with an `{agentId}` parameter
needs it. Omitting it means any enrolled agent can act as any other by
changing a URL segment.

## Enrollment: how an agent gets a `uca_` credential

An agent does not start with a credential; it proves an identity the platform
already vouches for and receives one.

On Kubernetes, that proof is a **projected ServiceAccount token**, which the
controller validates with a `TokenReview` against the API server, in the
shared `VerifyProjectedToken` (`kubernetes_token.go`) called from
`agent_enrollment_kubernetes.go`. The verification does six things, and every
one is load-bearing:

1. The review must report `Authenticated` **and** echo the expected audience —
   the API server's answer counts, not what was requested.
2. The token's own bound-Pod claims are parsed.
3. The reviewed subject must match those claims.
4. The ServiceAccount **UID** must match, so a token minted for a
   deleted-and-recreated ServiceAccount of the same name is rejected.
5. The Pod named in the claims is fetched live from the API server and
   compared field-by-field (UID, namespace, name, ServiceAccount) against the
   claims, so a token minted for a deleted-and-recreated **Pod** of the same
   name is rejected too — the UID check above catches a recycled
   ServiceAccount, this one catches a recycled Pod.
6. The enrollment policy's namespace and ServiceAccount constraints apply.

Items 1–5 live in `VerifyProjectedToken` and are shared with the
store-credential broker; item 6 is enrollment policy and is not.

!!! danger "The audiences must stay different"
    Enrollment verifies audience `unified-cd-agent-enrollment`. The
    store-credential broker verifies `unified-cd-store-credentials`. **If these
    were ever the same, any job Pod's token would enroll an agent.**

    This is asserted from two layers and in both directions, and the Pod-spec
    test pins the literal string rather than the constant so renaming cannot
    quietly make them equal. See
    [Invariants](invariants.md#the-two-token-audiences-must-stay-different).

## What an agent credential may not do

An enrolled `uca_` credential is **not** a general API token. It cannot create
runs — `POST /api/v1/runs` never accepts one. The boundary exists because an
agent executing user-supplied job code is a lower-trust caller than the human
who wrote the job.

If you add an endpoint that an agent needs, ask whether an agent that has been
handed a malicious job should be able to call it. If the answer is no, it does
not belong on `agentAuth`.

## The bootstrap token

`UNIFIED_TOKEN` is synced at startup as a PAT under the fixed name
`env:UNIFIED_TOKEN` (`BootstrapPATName`), so changing the value updates the
hash rather than accumulating rows. The name is **not** what grants break-glass:
`patPrincipal` compares the presented token's hash with `HashToken(Config.Token)`
(`Server.isBreakGlass`, constant-time) and requires that the PAT has no owner, and `handleCreateToken` rejects the
reserved `env:` prefix. At startup `SyncBootstrapPAT` deletes, in one
transaction, any other row with the reserved name and upserts the real one.

## Adding an endpoint

1. **Who calls it?** Human only → `ServerAuth`. Agent only → the matrix with
   `agentAuth`. Both → the matrix with `agentOrServerAuth` and an `AgentBypass`
   row in `routeTable`.
2. **Does the path contain `{agentId}`?** Then `bindPath: true`, always.
3. **Which permission, and on what scope?** Add the route to `routeTable` in
   `internal/controller/authz_routes.go` with a permission from the catalogue
   and a scope (a new permission goes into `internal/authz/catalogue.go` and
   the bundles first). If a handler learns the job from the body, call
   `requireJob` in it; if it returns rows, filter them with `canReadJob`.
4. **Does it mutate?** Then it belongs under a group with
   `auditLogMiddleware` and, to be recorded when allowed, in
   `auditActionTable` (denials are recorded regardless).
5. **Would an agent running a malicious job be able to abuse it?** If so it
   should not be reachable with `agentAuth` at all.

## Where to look

| Question | File |
|---|---|
| How is a human authenticated? | `internal/controller/auth.go` |
| How is OIDC handled? | `internal/controller/auth_oidc.go` |
| How do roles resolve, rank and union with `oidc.permissions`? | `internal/controller/rbac.go` |
| What is the permission catalogue, a role bundle, a folder match? | `internal/authz` |
| Which permission does each route need? | `routeTable` in `internal/controller/authz_routes.go` |
| Where is the check and the 404/403 split? | `authorize`, `requireJob` in `internal/controller/authz_decide.go` |
| How does the Web UI evaluate grants? | `web/src/lib/authz.js` (see [Frontend Development](frontend-development.md#permissions-in-the-ui)) |
| How is an agent authenticated? | `internal/controller/agent_auth.go` |
| Which agent routes exist, with what auth? | `agentRouteIdentityMatrix` in `server.go` |
| How does Kubernetes enrollment verify a token? | `agent_enrollment_kubernetes.go` |
