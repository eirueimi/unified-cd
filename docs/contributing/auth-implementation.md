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
for a PAT intersected with its ceiling.

### Permissions, not ranks

The decision logic lives in `internal/authz`, which has no dependency on the
controller:

- `catalogue.go` — the 30 permissions, which nine are folder-scopable, and the
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
matching `oidc.permissions` entries; login, `ServerAuth`, PAT accounts and
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
job Pod sidecar authentication on the presign endpoint; item 6 is enrollment policy and is not.

!!! danger "The audiences must stay different"
    Enrollment verifies audience `unified-cd-agent-enrollment`. The
    presign endpoint verifies `unified-cd-store-credentials` for job Pod sidecars. **If these
    were ever the same, any job Pod's token would enroll an agent.**

    This is asserted from two layers and in both directions, and the Pod-spec
    test pins the literal string rather than the constant so renaming cannot
    quietly make them equal. See
    [Invariants](invariants.md#the-two-token-audiences-must-stay-different).

## Object URL requesters

`POST /api/v1/runs/{runID}/objects/presign` (`api_objects.go`) hands out
15-minute presigned URLs for one object and one operation each. It is
registered outside every auth middleware (`unauthenticated` in `routeTable`)
because it accepts two kinds of caller, and `authenticateObjectRequester`
(`object_requester.go`) authenticates both itself:

- **An agent bearer (`uca_`).** It goes through `authenticateAgentBearer`, the
  same code `agentAuth` runs, is rate-limited by agent ID, and must then pass
  `agentRunGuard(..., false)` for the run in the path: the agent must have
  claimed the run. A finished run still gets URLs, as with
  `handleArtifactUpload`, so late uploads land.
- **Any other bearer: a job Pod sidecar's projected ServiceAccount token.** It
  is rate-limited by client address first (the `TokenReview` is a call to the
  cluster API), then verified by `verifyStoreCredentialToken` (audience
  `unified-cd-store-credentials`, `VerifyProjectedToken` items 1–5 above), then
  checked against the `StoreCredentialClusters` namespace and ServiceAccount
  allow-list. **The run binding is mandatory**: the Pod must be the one the
  run's `run_pod_bindings` row names. No row yet is `409 binding-pending` with
  `Retry-After: 1` (the k8s agent reports the binding on its next heartbeat);
  a different Pod is `403`. A run in a terminal state is refused with
  `409 run-finished` before the binding lookup (cancel, the reaper and the
  scheduler do not delete bindings), and the sidecar client does not retry it.
  A Cancelled run is the exception for `cancelledRunObjectGrace` (1 hour after
  `updated_at`) while Pods are still bound, because `finally:` and hook drains
  run after a cancel; a Cancelled run with no bindings left is `run-finished`.
  There is no permissive mode: the
  deprecated `storeCredentialRequireRunBinding` setting is ignored, because a URL is for one
  run's object.

Both rate limits use the `StoreCredentialRate*` settings. After
authentication the handler derives every key itself: artifacts as
`artifact.ArtifactKey(run, name)`, caches as `cache.DataKey(job, key)` where
the job is always the run record's (`store.RunJobName`), never a client value.
It never signs a cache `.meta` key; the cache `commit` operation writes `.meta`
server-side with the size from a `Stat` of the archive and an expiry bounded
by the run spec's own `cache.ttlDays`. Reading another run's artifact
(`sourceRunId`, `get`/`head` only) needs only that the run exists, which is the
existing download route's rule for machine principals. The URLs are bearer
credentials: never log or audit them.

## What an agent credential may not do

An enrolled `uca_` credential is **not** a general API token. It cannot create
runs — `POST /api/v1/runs` never accepts one. The boundary exists because an
agent executing user-supplied job code is a lower-trust caller than the human
who wrote the job.

If you add an endpoint that an agent needs, ask whether an agent that has been
handed a malicious job should be able to call it. If the answer is no, it does
not belong on `agentAuth`.

## Accounts and the initial token

Every principal is a row in `accounts` (a person keyed by OIDC `sub`, or a
service account keyed by name). `pats.account_id` and `sessions.account_id` are
`NOT NULL` (migration 037), so a PAT or session can never exist without an
account, and the former `owner_*` columns on `pats` no longer exist: the owner
shown in the token list comes from the account (email, else `sub`, else
`sa:<name>`).

There is no static controller token. A fresh install is bootstrapped with the
one-time initial token (`initial-admin-token`, or `UNIFIED_INITIAL_ADMIN_TOKEN`),
which `handleBootstrap` exchanges for the first PAT of the built-in `admin`
service account. `handleBootstrap` is rate limited per client address (5 per
minute) and refuses with 409, without consuming the token, while `admin` is
disabled. `handleCreateToken` rejects the reserved `env:` and `system:`
name prefixes.

Authentication resolves a credential to an account first and the permissions
second:

- `ServerAuth` (`auth.go`) tries the PAT, the OIDC bearer id_token and the
  session cookie. `patPrincipal` and `sessionPrincipal` call `resolveAccount`
  (`auth_accounts.go`), which answers `forbidden: account disabled` for a
  disabled or unknown account and `forbidden: no role` for one that holds
  nothing; a failed account lookup is a 500 `account store error`
  (`refusalStatus`), never "disabled". A human's grants come from `identityGrants` over the account's stored
  email, `sub` and claims; a service account's from `serviceAccounts` in the
  config (compiled by `authz.CompileServiceAccount`); the built-in `admin`
  holds every permission while it is enabled (`adminEnabled` also honours the
  `admin_enabled_until` lease that `admin recover --enable-admin` writes).
- The bearer id_token path upserts the human account at most once a minute per
  `sub` (`idTokenAccount`), so a disable reaches other replicas' id_token
  requests within that minute. Sessions and PATs re-read the account on every
  request.
- With browser SSO configured (OIDC with a client secret), a PAT on a request
  that carries `Sec-Fetch-Site` is refused (`msgBrowserPATRefused`) unless
  `ui.allowTokenEntry` is set. The check keys on `Sec-Fetch-Site` because Node's
  fetch sends `Sec-Fetch-Mode` but not `Sec-Fetch-Site`. This is a UI policy,
  not a boundary (`allowBrowserTokenEntry`, `api_uiconfig.go`).
- `patExpiry` (`api_tokens.go`) applies `tokens.defaultLifetime`,
  `tokens.maxLifetime` and the 720h cap of the `admin` account. A PAT may only
  mint PATs for its own account unless the caller holds `token.manage` or
  `serviceaccount.token`, and never for `admin`. `serviceaccount.token` also lets a
  caller list and delete the PATs of service accounts other than `admin`
  (`serviceAccountTokenScope`); the token routes carry it as `AltPerm` (create) or
  `AltPerm2` (list, delete).
- `<controller binary> admin recover` (`cmd/controller/recover.go`) is not an
  HTTP route: it opens the database with the controller's configuration,
  creates a PAT of `admin`, writes the audit row with actor `recovery`, and
  only then (optionally) writes the lease. It is the emergency path, so keep it
  free of the HTTP stack.
- Audit identities: `pat:<id>`, `oidc:<sub>`, `bootstrap`, `recovery`,
  `migration`; a service account's PAT rows carry `details.account =
  "sa:<name>"` (`auditLogMiddleware`).

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
   `TestEveryHumanWriteRouteIsAuditedOrExempt` fails for a human write route
   that is in neither `auditActionTable` nor the commented exemption set
   `auditExemptWriteRoutes` (`audit_routes_test.go`).
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
