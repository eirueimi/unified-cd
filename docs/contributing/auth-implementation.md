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
| Carries | Name, Kind, **Role** | an agent identity — no role |
| In context under | `principalCtxKey` | its own key |

They are deliberately separate. An agent is not a low-privileged user; it is a
different kind of caller with a different question to answer ("which agent is
this, and is it allowed to act for *that* run?"), so giving it a role would be
answering the wrong question.

`Principal.Kind` is `"pat" | "oidc" | "session"`, and `Principal.Role` is
`"admin" | "developer" | "viewer"`.

### The role hierarchy is a rank, not a set

`roleRanks` in `rbac.go` is `viewer: 1, developer: 2, admin: 3`, and
`requireMinRole` compares ranks. An unknown or empty role ranks **0**, so it
fails every check — which is the right default and worth preserving if you
touch `roleRank`.

`resolveRole` maps OIDC claims to a role in a fixed order: `userMap` by email,
then `userMap` by sub, then the highest-ranked match in `roleMap`, then the
default. A denial returns `("", false)` rather than a role that happens to rank
zero, so a deny is explicit rather than emergent.

## Which middleware guards what

Four middlewares matter, and picking the wrong one is the mistake to avoid.

| Middleware | Admits | Use for |
|---|---|---|
| `ServerAuth` | humans and API clients only | ordinary API routes |
| `agentAuth` | `uca_` agent credentials only | agent-only routes |
| `agentOrServerAuth` | either | routes both an agent and a human may call |
| `viewerOrAgent` | an agent, else requires the viewer role | pair with `agentOrServerAuth` on **read** routes |

`agentOrServerAuth` and `viewerOrAgent` are used together, not separately:
the first establishes *some* principal, the second decides whether a human one
is sufficiently privileged. Using `agentOrServerAuth` alone on a route means
any authenticated human, at any role, gets in.

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
hash rather than accumulating rows.

## Adding an endpoint

1. **Who calls it?** Human only → `ServerAuth`. Agent only → the matrix with
   `agentAuth`. Both → the matrix with `agentOrServerAuth`, plus
   `viewerOrAgent` if it is a read.
2. **Does the path contain `{agentId}`?** Then `bindPath: true`, always.
3. **What is the minimum role?** `requireMinRole` — and remember an unknown
   role ranks 0 and is denied.
4. **Does it mutate?** Then it belongs under a group with
   `auditLogMiddleware`.
5. **Would an agent running a malicious job be able to abuse it?** If so it
   should not be reachable with `agentAuth` at all.

## Where to look

| Question | File |
|---|---|
| How is a human authenticated? | `internal/controller/auth.go` |
| How is OIDC handled? | `internal/controller/auth_oidc.go` |
| How do roles resolve and rank? | `internal/controller/rbac.go` |
| How is an agent authenticated? | `internal/controller/agent_auth.go` |
| Which agent routes exist, with what auth? | `agentRouteIdentityMatrix` in `server.go` |
| How does Kubernetes enrollment verify a token? | `agent_enrollment_kubernetes.go` |
