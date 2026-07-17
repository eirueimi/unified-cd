# Per-Agent Identity and Automated Enrollment Design

**Date:** 2026-07-17
**Status:** Approved for planning

## Summary

Replace the shared agent bearer token with a persistent agent identity and a
short-lived, per-agent opaque access token. Automate initial enrollment through
two providers:

- a one-time enrollment token for VM, physical, and recovery workflows; and
- a Kubernetes projected ServiceAccount token validated through TokenReview for
  ephemeral agent Pods.

The controller is the only component that issues unified-cd credentials. The
CLI requests administrative enrollment material from the controller but never
generates credentials offline. Agent authorization is based on an
`AgentPrincipal` derived from the credential, not on an agent ID supplied in a
URL or request body.

The first delivery uses bearer access tokens. The enrollment and principal
boundaries are deliberately transport-neutral so a later delivery can issue
short-lived client certificates and use mTLS without rewriting agent API
authorization.

## Context

The current controller has one `AgentToken` value. Every agent-facing route uses
`BearerAuth(s.cfg.AgentToken)`, and the controller entry point currently supplies
the main bootstrap token as both `Token` and `AgentToken`. The middleware proves
only that the caller knows the fleet-wide secret; it does not establish which
agent is calling. Run ownership guards reduce the impact on some write paths,
but the authentication layer still cannot bind a caller to `{agentId}`.

The existing `agents` table represents live registration and presence. An agent
can deregister by deleting that row. Authentication identity must survive a
normal deregistration or process restart, so credentials must not be owned by
the live `agents` row.

The target deployment includes both:

- long-lived VM or physical agents; and
- ephemeral Kubernetes agent Pods, each with an individual identity and
  short-lived credential.

The root development Compose file is not a production deployment contract. The
design applies equally to direct controller TLS, an L4 load balancer, or a
trusted production ingress.

## Goals

- Give every agent an independently revocable identity and credential.
- Bind every agent API call to the authenticated agent identity.
- Remove fleet-wide runtime credentials from normal operation.
- Automate Kubernetes Pod enrollment without a shared Kubernetes Secret.
- Support unattended VM operation, including restart after an access token has
  expired.
- Work with multiple stateless controller replicas backed by PostgreSQL.
- Keep secrets out of the database, logs, process arguments, and API list
  responses.
- Reuse the enrollment and authorization model for optional mTLS later.
- Provide an explicit, auditable migration from the legacy shared token.

## Non-goals

- Operating a general-purpose certificate authority in the bearer-token phase.
- Replacing human PAT, OIDC, session, or RBAC authentication.
- Implementing cloud-specific workload identity providers in the first
  delivery.
- Solving build-to-build isolation or host hardening through authentication.
- Changing root `docker-compose.yaml` into a production deployment model.
- Granting an agent unrestricted labels or capabilities based on self-reported
  registration data.

## Decisions

### 1. Controller-issued opaque credentials

The controller generates all unified-cd credentials using a cryptographically
secure random source. Tokens are opaque, contain at least 256 bits of random
secret material, and have a typed prefix and a non-secret lookup ID:

```text
uca_<credential-id>_<secret>   # agent access token
ucr_<credential-id>_<secret>   # VM recovery/refresh credential
uce_<enrollment-id>_<secret>   # one-time enrollment token
```

Only a SHA-256 hash of the complete high-entropy token is stored. The lookup ID
selects one candidate row; the server then compares the stored and presented
hashes in constant time. Tokens are returned exactly once at creation and are
never returned by list or detail APIs.

Opaque credentials are preferred over signed JWT access tokens because every
request already depends on PostgreSQL-backed controller state, immediate
per-agent revocation is a core requirement, and avoiding a shared JWT signing
key reduces controller key-management scope.

### 2. Separate persistent identity from live presence

Add a persistent `agent_identities` record. It remains when the live `agents`
row is deleted and is the parent of credentials and enrollment bindings.

```text
agent_identities
  id                 UUID primary key
  agent_id           TEXT unique, human-visible canonical agent ID
  status             TEXT: active | disabled
  enrollment_method  TEXT
  external_subject   TEXT nullable
  created_at         TIMESTAMPTZ
  disabled_at        TIMESTAMPTZ nullable
  last_authenticated_at TIMESTAMPTZ nullable
```

The internal UUID is immutable. The canonical `agent_id` is used by existing
agent APIs and run ownership. A future certificate identifies the immutable UUID
and the controller resolves it to the canonical agent ID.

Non-null external subjects are unique within their enrollment method. A
Kubernetes subject includes the verified cluster key and Pod UID rather than
only the shared ServiceAccount name.

Disabling an identity immediately invalidates all of its access and refresh
credentials. Deregistering a live agent only removes presence information and
does not implicitly revoke identity.

### 3. Access and recovery credentials

Add credentials separate from enrollment material:

```text
agent_credentials
  id                 UUID primary key
  identity_id        UUID references agent_identities(id)
  kind               TEXT: access | refresh
  family_id          UUID nullable; required for refresh credentials
  token_hash         TEXT
  expires_at         TIMESTAMPTZ
  revoked_at         TIMESTAMPTZ nullable
  replaced_by        UUID nullable
  superseded_at      TIMESTAMPTZ nullable
  overlap_expires_at TIMESTAMPTZ nullable
  created_at         TIMESTAMPTZ
  last_used_at       TIMESTAMPTZ nullable
```

Access-token defaults:

- lifetime: 1 hour;
- replacement starts when 15 minutes remain, with per-agent jitter;
- a newly issued token does not revoke the previous token; the previous token
  remains valid until its original expiry to tolerate races and in-flight
  requests; and
- enrollment policy may lower the lifetime or raise it up to an operator-set
  maximum of 24 hours.

An access token cannot issue its replacement. This prevents a stolen access
token from extending its own lifetime indefinitely. A VM presents its refresh
credential, and a Kubernetes Pod presents its projected ServiceAccount token,
whenever it needs a replacement access token.

VM agents also receive a scoped refresh credential with a default 30-day
lifetime. It can call only the refresh endpoint and exists so a VM that was
offline longer than the access-token lifetime can recover without an operator.
It is stored on disk with restrictive permissions and is rotated whenever it is
used, including routine access-token replacement. The previous refresh
credential remains valid for a five-minute
crash-recovery overlap; use after the overlap is treated as suspected replay and
revokes the refresh family. Expiry or revocation requires a new one-time
enrollment.

If an agent retries the superseded refresh credential inside the overlap, the
controller revokes the replacement created by the apparently lost response and
issues a new pair. It never needs to retain or return a prior plaintext token.

Kubernetes agents do not receive a refresh credential. Both replacement and
restart require the Pod to prove its bound ServiceAccount identity again. The
access token remains in memory and disappears with the Pod.

### 4. A single AgentPrincipal authorization boundary

Replace `BearerAuth` on agent routes with `AgentAuth`. Successful authentication
adds this value to request context:

```go
type AgentPrincipal struct {
	IdentityID  string
	AgentID     string
	CredentialID string
	AuthMethod  string // "bearer" now; "mtls" later
}
```

Every `/api/v1/agents/{agentId}/...` handler or route middleware must require:

```text
AgentPrincipal.AgentID == path agentId
```

`POST /api/v1/agents/register` must require the body `agentId` to equal the
principal. An agent may never select or rename its authenticated identity by
changing a path, body, hostname, label, or environment field.

Run-scoped writes continue to enforce immutable `runs.claimed_by` ownership in
addition to the principal/path check. Artifact upload has no `{agentId}` path,
so it must compare the authenticated principal with the target run's
`claimed_by`. Human artifact reads continue through `ServerAuth`.

Cross-run artifact download authorization is not broadened by this design. The
implementation plan must preserve currently required dependency-download
behavior and add a separate capability-based design if agents need narrower
cross-run reads; it must not silently assume that a per-agent identity alone
authorizes every artifact.

### 5. Enrollment providers and policies

Enrollment is a trust exchange, not normal agent authentication. Define a small
provider interface that returns a verified external enrollment principal:

```go
type EnrollmentPrincipal struct {
	Provider string
	Subject  string
	Claims   map[string]string
}

type EnrollmentProvider interface {
	Authenticate(ctx context.Context, request CredentialInput) (EnrollmentPrincipal, error)
}
```

An `EnrollmentPolicy` maps a verified principal to allowed identity attributes:

```text
agent_enrollment_policies
  id                    UUID primary key
  name                  TEXT unique
  provider              TEXT
  provider_config       JSONB
  subject_constraints   JSONB
  agent_id_template     TEXT
  allowed_labels        TEXT[]
  required_labels       TEXT[]
  access_token_ttl      INTERVAL
  enabled               BOOLEAN
  created_at            TIMESTAMPTZ
  updated_at            TIMESTAMPTZ
```

The controller derives the canonical agent ID and authoritative labels from the
verified principal plus policy. Client-provided labels are intersected with the
policy allowlist. Security-sensitive routing labels and capabilities cannot be
self-asserted.

The initial delivery implements exactly two providers.

#### One-time token provider

An administrator creates a token scoped to one fixed canonical agent ID:

```text
agent_enrollment_tokens
  id                 UUID primary key
  agent_id           TEXT
  token_hash         TEXT
  expires_at         TIMESTAMPTZ
  used_at            TIMESTAMPTZ nullable
  revoked_at         TIMESTAMPTZ nullable
  created_by         TEXT
  created_at         TIMESTAMPTZ
```

The default lifetime is 10 minutes. Exchange is single-use and atomic: the
controller locks the row, verifies hash, expiry, unused and unrevoked state,
creates the intended identity or verifies that an existing identity is active,
creates credentials, marks the enrollment token used, and commits in one
transaction. A disabled identity requires a separate administrative enable
operation and cannot be reactivated by enrollment.

This provider is used for VM and physical agents, initial setup, and recovery.

#### Kubernetes ServiceAccount provider

Each trusted Kubernetes cluster has controller-side provider configuration and
an enrollment policy. An agent Pod presents a projected, Pod-bound
ServiceAccount token with audience `unified-cd-agent-enrollment`.

`provider_config` stores only non-secret cluster metadata and a reference to
controller-side cluster credentials. Kubeconfig bearer tokens, client keys, and
other cluster secrets are supplied through the deployment secret mechanism and
are not stored in policy JSON.

The controller:

1. sends the token to the configured cluster's TokenReview API with the required
   audience;
2. requires the expected ServiceAccount subject and namespace;
3. requires a Pod-bound token and verifies the bound Pod name, UID, and
   ServiceAccount against the cluster API;
4. derives the canonical agent ID from trusted values, for example
   `k8s:<cluster-key>:<namespace>:<pod-uid>`;
5. applies policy-controlled labels and token lifetime; and
6. issues one access token and no refresh credential.

The controller never chooses a cluster solely from unverified JWT claims. The
request selects a configured provider key, and that provider validates the token
against its own cluster API. A cluster API outage blocks new enrollment through
that provider but does not invalidate an already issued access token before its
expiry.

### 6. CLI is a client, not an issuer

The administrative CLI requests enrollment tokens from the controller:

```bash
unified-cd agent enrollment create \
  --agent-id vm-agent-01 \
  --expires-in 10m \
  --output-file /secure/path/enrollment-token
```

The command calls an admin-only API using normal human authentication. The
controller generates and stores the credential. The CLI displays or writes the
secret once and must warn when outputting to a terminal. `--output-file` creates
the file with restrictive permissions where the platform supports them.

The CLI must not generate credentials offline, possess a controller signing
key, connect directly to PostgreSQL, or retrieve agent access/refresh tokens.
Access and refresh credentials are returned only to the enrolling agent.

Kubernetes enrollment policies are also managed through controller-backed CLI
commands, but the CLI is not invoked once per Pod.

## API Surface

Names below are the intended public contract; the implementation plan must use
these names consistently in code, docs, examples, and generated references.

### Administrative endpoints

```text
POST   /api/v1/agent-enrollments
GET    /api/v1/agent-enrollments
DELETE /api/v1/agent-enrollments/{id}

POST   /api/v1/agent-enrollment-policies
GET    /api/v1/agent-enrollment-policies
GET    /api/v1/agent-enrollment-policies/{name}
PUT    /api/v1/agent-enrollment-policies/{name}
DELETE /api/v1/agent-enrollment-policies/{name}

POST   /api/v1/agent-identities/{agentId}/disable
POST   /api/v1/agent-identities/{agentId}/enable
POST   /api/v1/agent-identities/{agentId}/credentials/revoke
```

Issuing, modifying, enabling, disabling, and revoking require the `admin` role.
Read-only identity and policy metadata requires at least `viewer`; no secret or
hash is included.

### Bootstrap and token endpoints

```text
POST /api/v1/agents/enroll
POST /api/v1/agents/token/refresh
```

`enroll` accepts a provider key and provider credential. One-time tokens and
Kubernetes ServiceAccount tokens are carried in the Authorization header and
must never be present in the URL. The response contains the access token and its
expiry. The one-time provider additionally returns a refresh credential and its
expiry.

`refresh` accepts a VM refresh credential and returns a replacement refresh
credential plus a new access token. Neither request accepts an agent ID; identity
comes exclusively from the presented credential.

The Kubernetes provider uses `enroll` for both initial issuance and replacement.
The verified cluster, namespace, ServiceAccount, and Pod UID resolve to the
existing identity after the first exchange.

### Existing agent endpoints

All existing agent routes switch from `BearerAuth(sharedToken)` to `AgentAuth`.
Existing human list/detail routes remain under `ServerAuth` and RBAC.

## End-to-end Flows

### VM or physical agent

```text
Admin CLI             Controller                 VM agent
    | POST enrollment     |                          |
    |-------------------->|                          |
    | one-time token      |                          |
    |<--------------------|                          |
    | deliver securely ---------------------------->|
    |                     |<----- enroll(token) -----|
    |                     | create identity, access  |
    |                     | and refresh credentials  |
    |                     |------ access+refresh --->|
    |                     |<----- register(access) --|
    |                     |                          |
    |                     |<----- refresh(refresh) --|
    |                     |--- new access+refresh -->|
```

The VM stores only the refresh credential persistently. It keeps the access
token in memory when practical. Credential-file replacement is atomic and uses
owner-only permissions on Unix and a restricted ACL on Windows.

### Ephemeral Kubernetes agent Pod

```text
Pod             Controller             Kubernetes API
 | enroll(SA JWT)   |                         |
 |----------------->| TokenReview(audience)   |
 |                  |------------------------>|
 |                  | authenticated identity  |
 |                  |<------------------------|
 |                  | verify bound Pod/UID    |
 |                  |------------------------>|
 |                  |<------------------------|
 | access token     |                         |
 |<-----------------|                         |
 | register(access) |                         |
 |----------------->|                         |
```

Pod restart creates a different Pod UID and therefore a different agent
identity. The previous identity naturally stops heartbeating and is handled by
existing stale-agent/run reconciliation. A retention task may later prune old,
credential-free ephemeral identities after the audit retention period.

## HA and Consistency

- Any controller replica can issue or validate opaque credentials because
  identity, credential, and enrollment state is in PostgreSQL.
- Enrollment consumption, identity creation, and credential issuance are one
  database transaction.
- Replacement inserts the new access token and, for a VM, the rotated refresh
  credential before returning them. The prior access token remains valid until
  expiry and the prior refresh credential has a five-minute retry overlap.
- Identity disable and credential revoke take effect on the next request because
  `AgentAuth` checks database state; no replica-local authorization cache is used
  initially.
- Authentication fails closed when PostgreSQL is unavailable.
- `last_used_at` is observability data and may be updated asynchronously; it is
  never part of the authorization decision.

## Security Controls

- Production agent enrollment and APIs require HTTPS. Plain HTTP is permitted
  only through an explicit development-only setting.
- Authorization headers and credential response bodies are redacted from logs,
  traces, metrics labels, and error messages.
- Enrollment and refresh endpoints have per-source and per-policy rate limits.
- Authentication errors are generic externally; structured audit events retain
  a non-secret reason code.
- The controller derives identity before accepting registration metadata.
- Enrollment policy controls labels, capabilities, token TTL, namespace, and
  ServiceAccount scope.
- One-time enrollment consumption is atomic and replay-safe.
- Refresh-token rotation detects reuse outside its short crash-recovery overlap.
- Disabling an identity revokes all current access in one operation.
- Agent secrets-fetch and run-write handlers retain their claimed-run ownership
  checks in addition to identity binding.
- Configuration rejects an unrestricted Kubernetes enrollment policy that has
  neither namespace nor ServiceAccount constraints.
- Kubernetes provider credentials receive only the TokenReview permission and
  the Pod-read scope required by configured namespaces; they are not cluster
  administrator credentials.

## Audit Events and Metrics

Record audit events for:

- enrollment token creation, use, revocation, expiry, and replay;
- external enrollment success and failure;
- identity enable and disable;
- access replacement and refresh recovery;
- credential revocation and suspected refresh replay; and
- principal/path or principal/run ownership mismatch.

Events include identity UUID, canonical agent ID when known, provider, policy,
credential lookup ID, actor, source address, and reason code. They never include
tokens, token hashes, ServiceAccount JWTs, or certificate private material.

Add bounded-cardinality counters by provider, result, and reason. Do not use
agent ID, Pod UID, or credential ID as metric labels.

## Legacy Shared-token Migration

The shared token must not silently remain a permanent second authentication
path. Migration is explicit:

1. Ship per-agent identity and enrollment while retaining legacy mode behind an
   explicit `agentAuth.legacySharedToken` compatibility setting.
2. In compatibility mode, emit a startup warning and a metric showing legacy
   authentication usage.
3. Enroll VM agents with one-time tokens and roll Kubernetes agents with the
   ServiceAccount provider.
4. Operators confirm zero legacy-auth requests for a full agent rollout window.
5. Remove the compatibility setting and shared secret from the deployment.

New installations default to per-agent authentication and do not synthesize
`AgentToken` from the human bootstrap token. Existing shared-token runtime calls
remain fleet-wide authority while compatibility mode is enabled; documentation
must state that this is a temporary migration risk, not an equivalent secure
mode.

## Future mTLS Phase

The later mTLS phase reuses enrollment providers and `AgentPrincipal`:

1. the agent generates its private key locally;
2. it submits a CSR while authenticated through an enrollment provider or valid
   existing agent credential;
3. the controller or configured external CA issues a short-lived client
   certificate;
4. the URI SAN identifies the immutable identity UUID as
   `spiffe://<configured-trust-domain>/agent/<identity-uuid>`;
5. TLS authentication resolves the UUID to the same `AgentPrincipal`; and
6. all existing path and run-ownership authorization remains unchanged.

The private key never leaves the agent. Certificate renewal is automatic.

For direct controller termination, including an L4 load balancer with TLS
passthrough, the controller validates the client chain, validity period, client
authentication key usage, and the single expected URI SAN. It resolves the
identity UUID and creates `AgentPrincipal{AuthMethod: "mtls"}` before routing the
request. This is the preferred production boundary because certificate identity
reaches the authorization layer without an HTTP identity-header hop.

For L7 ingress termination, the ingress performs the same client-certificate
validation and forwards only the normalized identity UUID. It must remove any
client-supplied identity header, connect to a dedicated controller listener, and
authenticate that hop with pinned mTLS or an equivalently strong private
transport identity. The controller rejects identity headers on its general
listener and trusts them only on that dedicated authenticated ingress path.
Revocation and identity-disable checks still occur in the controller database.

Bearer access tokens remain the first delivery and mTLS remains an optional
hardening mode, not a prerequisite for automated enrollment.

## Failure Handling

- **Invalid, expired, or revoked access token:** return `401` with a stable
  machine-readable code; the agent attempts refresh or external re-enrollment as
  appropriate.
- **Authenticated agent ID mismatch:** return `403`; never rewrite the requested
  ID to make the request succeed.
- **Expired one-time enrollment:** return `401`; an administrator creates a new
  token.
- **Already-used enrollment token:** return generic `401`, record a replay audit
  event, and do not reveal which check failed.
- **Kubernetes TokenReview unavailable:** return retryable `503`; the agent uses
  exponential backoff with jitter and may continue with its current access token
  until that token expires.
- **Policy mismatch:** return `403` with a stable non-secret reason code and audit
  the verified subject.
- **Lost replacement response:** the old access token remains valid until its
  original expiry. A VM retries with the previous refresh credential during its
  five-minute overlap; Kubernetes retries with the same bound ServiceAccount
  token.
- **VM refresh replay:** revoke the refresh family and require re-enrollment.
- **Database unavailable:** fail authentication and issuance closed with `503`;
  never fall back to the legacy token unless compatibility mode was explicitly
  configured.

## Testing Strategy

### Unit tests

- token format, entropy source failure, hashing, constant-time verification, and
  secret redaction;
- access expiry, overlap, identity disable, credential revoke, and refresh-family
  replay;
- prove an access token cannot obtain another access token;
- principal/path and principal/body mismatch;
- policy matching, label allowlisting, TTL bounds, and canonical ID derivation;
- one-time token atomic consumption; and
- Kubernetes provider audience, subject, Pod binding, UID, and policy checks with
  a fake TokenReview/Pod API.

### Controller integration tests

- enroll, register, heartbeat, claim, report, upload, Kubernetes re-enrollment,
  VM refresh, revoke, and disable across two controller instances sharing
  PostgreSQL;
- prove agent A cannot call any `{agentId}` route for agent B;
- prove agent A cannot write to a run claimed by agent B, including artifact
  upload routes without `{agentId}`;
- concurrent exchange of one enrollment token yields exactly one success;
- a lost replacement response leaves a usable retry path; and
- database and Kubernetes API failures fail closed with the documented status.

### Agent tests

- VM credential file creation, permissions/ACL, atomic rotation, restart recovery,
  and redaction;
- Kubernetes Pod enrollment, in-memory access token, replacement jitter, and
  re-enrollment after restart;
- access expiry during long-poll and request retry behavior; and
- migration from configured legacy token to per-agent enrollment.

### Documentation and configuration checks

- update authentication, authorization, agents, configuration, Kubernetes,
  operations, troubleshooting, CLI, and migration documentation;
- update manifests, examples, and templates wherever agent credentials are
  configured;
- document that the root Compose file is development-only and does not define
  production TLS termination;
- reject copied examples that still configure one runtime token for every agent;
  and
- include exact, grep-able errors for expiry, policy rejection, identity mismatch,
  and refresh replay.

## Acceptance Criteria

- Two agents never share a runtime credential in normal mode.
- Possession of agent A's credential cannot authenticate as agent B.
- Revoking or disabling one identity does not interrupt other agents.
- A Kubernetes Pod enrolls unattended using its bound ServiceAccount token and
  receives a Pod-specific access token.
- A VM enrolls once, renews unattended, and can restart after access-token expiry
  using its scoped refresh credential.
- No plaintext unified-cd credential is stored in PostgreSQL or returned after
  its creation response.
- All controller replicas issue and validate consistently through PostgreSQL.
- Legacy shared-token use is opt-in, observable, and removable.
- The authorization layer consumes `AgentPrincipal` and does not depend on the
  bearer mechanism, allowing mTLS to be added without handler rewrites.
