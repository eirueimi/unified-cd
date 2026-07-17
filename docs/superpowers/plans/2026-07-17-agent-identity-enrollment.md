# Per-Agent Identity and Automated Enrollment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the fleet-wide runtime agent secret with independently revocable, short-lived opaque credentials, one-time VM enrollment, and automatic Kubernetes ServiceAccount enrollment.

**Architecture:** PostgreSQL owns persistent identities, hashed credentials, one-time enrollment records, and Kubernetes enrollment policies. Controller middleware resolves every credential into one `AgentPrincipal`; VM agents use a rotating refresh credential, while Kubernetes Pods repeatedly prove their bound ServiceAccount identity to obtain access tokens.

**Tech Stack:** Go 1.26.2, PostgreSQL 16, golang-migrate, chi v5, Cobra, Prometheus client, Kubernetes API/client-go v0.36.1, testify, dockertest.

**Design:** `docs/superpowers/specs/2026-07-17-agent-identity-enrollment-design.md`

## Global Constraints

- Implement the bearer-token phase only. mTLS certificate issuance and TLS termination changes require a separate future plan.
- The root `docker-compose.yaml` remains development-only; it is not a production deployment contract.
- New installations never derive agent authorization from `UNIFIED_TOKEN`. Legacy shared-agent auth exists only through `agentAuth.legacySharedToken` / `UNIFIED_AGENT_LEGACY_TOKEN`.
- Formats are `uca_<uuid>_<secret>`, `ucr_<uuid>_<secret>`, and `uce_<uuid>_<secret>`, with 32 random bytes encoded base64url without padding.
- Store SHA-256 hashes only. Never log, audit, metric-label, URL-encode, or list plaintext credentials.
- Access tokens default to 1 hour, are bounded to 5 minutes–24 hours, and start replacement with 15 minutes plus jitter remaining. Access tokens cannot issue access tokens.
- VM refresh credentials default to 30 days, rotate on every use, and have a 5-minute crash-retry overlap. Kubernetes agents receive no refresh credential.
- Non-legacy requests must bind `AgentPrincipal.AgentID` to the route/body agent ID and retain immutable `runs.claimed_by` checks.
- Labels and capabilities are controller-authorized identity properties. Non-legacy agents cannot self-assert scheduling authority.
- Kubernetes enrollment requires audience `unified-cd-agent-enrollment`, successful TokenReview, and a live bound Pod with matching UID and ServiceAccount.
- Auth fails closed on PostgreSQL errors. TokenReview availability failures are retryable `503`.
- Metrics have bounded labels; agent IDs, Pod UIDs, credential IDs, subjects, and IPs are never metric labels.
- All repository text is English and contains no PII. Follow TDD and make the commit named in each task only after its tests pass.

## File Map

New focused units:

- `internal/agentauth/token.go` — opaque token codec and constant-time verification.
- `internal/api/agent_auth.go` — enrollment, identity, refresh, and policy JSON DTOs.
- `internal/store/migrations/013_agent_identity_auth.*.sql` — identity/auth schema.
- `internal/store/postgres_agent_auth.go` — credential CRUD and transactional issuance/rotation.
- `internal/controller/agent_auth.go` — `AgentPrincipal`, per-agent middleware, and explicit legacy fallback.
- `internal/controller/api_agent_enrollment.go` — admin lifecycle, one-time exchange, and VM refresh.
- `internal/controller/enrollment_limiter.go` — bounded source/policy limiter.
- `internal/controller/agent_enrollment_kubernetes.go` — TokenReview and bound-Pod validation.
- `internal/cli/agent_enrollment.go` — admin enrollment/identity/policy commands.
- `internal/agent/token_source.go` and `credentials.go` — VM runtime credential manager.
- `internal/k8sagent/credentials.go` — projected-ServiceAccount token source.
- `docs/migration-agent-auth.md` — operator rollout/rollback guide.

Existing files changed together:

- Store: `internal/store/store.go`, `verify.go`, their tests.
- Controller: `server.go`, `api_agent.go`, `api_artifacts.go`, `audit.go`, tests.
- Runtime/config: `cmd/controller/main.go`, `cmd/agent/main.go`, `cmd/k8s-agent/main.go`, `internal/config/*.go`.
- CLI/agent: `internal/cli/agent_install.go`, `internal/agent/client.go`, tests.
- Kubernetes: `internal/k8sagent/config.go`, `manifests/base/controller/*`, `manifests/base/k8s-agent/*`, aggregate manifests.
- Operations: authentication, authorization, agent, configuration, Kubernetes, operations, troubleshooting, audit, CLI, getting-started, README, examples, and templates documentation.

---

### Task 1: Token codec and migration 013

**Files:**
- Create: `internal/agentauth/token.go`
- Create: `internal/agentauth/token_test.go`
- Create: `internal/store/migrations/013_agent_identity_auth.up.sql`
- Create: `internal/store/migrations/013_agent_identity_auth.down.sql`
- Modify: `internal/store/verify.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Produces: `Generate(TokenKind) (IssuedToken, error)`, `Parse(string, TokenKind) (ParsedToken, error)`, `Hash(string) string`, `Matches(string, string) bool`.
- Produces: four PostgreSQL tables consumed by Task 2.

- [ ] **Step 1: Write the failing token tests**

Create tests that generate each kind, parse it, assert the UUID and 32-byte decoded secret, match the stored hash, reject mutation, reject the wrong kind, and reject malformed UUID/base64/length:

```go
func TestGenerateParseAndMatch(t *testing.T) {
	for _, kind := range []TokenKind{AccessToken, RefreshToken, EnrollmentToken} {
		issued, err := Generate(kind)
		require.NoError(t, err)
		parsed, err := Parse(issued.Plaintext, kind)
		require.NoError(t, err)
		assert.Equal(t, issued.ID, parsed.ID)
		assert.True(t, Matches(issued.Plaintext, issued.Hash))
		assert.False(t, Matches(issued.Plaintext+"x", issued.Hash))
	}
}

func TestParseRejectsWrongKind(t *testing.T) {
	issued, err := Generate(AccessToken)
	require.NoError(t, err)
	_, err = Parse(issued.Plaintext, RefreshToken)
	require.ErrorContains(t, err, "unexpected token type")
}
```

- [ ] **Step 2: Run the red test**

Run: `go test ./internal/agentauth -count=1`

Expected: FAIL because the package/types do not exist.

- [ ] **Step 3: Implement the codec**

Use these exact public definitions:

```go
type TokenKind string

const (
	AccessToken TokenKind = "uca"
	RefreshToken TokenKind = "ucr"
	EnrollmentToken TokenKind = "uce"
)

type IssuedToken struct { ID, Plaintext, Hash string }
type ParsedToken struct { ID, Secret string }
```

`Generate` uses `uuid.NewString()`, `crypto/rand.Read` into 32 bytes, and `base64.RawURLEncoding`. `Parse` uses exactly three underscore-delimited parts, validates `uuid.Parse`, decodes exactly 32 bytes, and reports only generic format errors. `Matches` decodes both SHA-256 hex values and calls `subtle.ConstantTimeCompare`. Run `go mod tidy`; promote the already-present `github.com/google/uuid v1.6.0` without changing its version.

- [ ] **Step 4: Run token tests green**

Run: `go test ./internal/agentauth -count=1`

Expected: PASS.

- [ ] **Step 5: Add migration 013**

The up migration creates:

```sql
CREATE TABLE public.agent_identities (
  id uuid DEFAULT gen_random_uuid() PRIMARY KEY,
  agent_id text NOT NULL UNIQUE,
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  enrollment_method text NOT NULL,
  external_subject text,
  authorized_labels text[] NOT NULL DEFAULT '{}',
  authorized_capabilities text[] NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now(),
  disabled_at timestamptz,
  last_authenticated_at timestamptz
);
CREATE UNIQUE INDEX agent_identities_external_subject_idx
  ON public.agent_identities(enrollment_method, external_subject)
  WHERE external_subject IS NOT NULL;

CREATE TABLE public.agent_credentials (
  id uuid PRIMARY KEY,
  identity_id uuid NOT NULL REFERENCES public.agent_identities(id) ON DELETE CASCADE,
  kind text NOT NULL CHECK (kind IN ('access','refresh')),
  family_id uuid,
  generation integer NOT NULL DEFAULT 0 CHECK (generation >= 0),
  token_hash text NOT NULL UNIQUE CHECK (length(token_hash)=64),
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz,
  replaced_by uuid REFERENCES public.agent_credentials(id),
  superseded_at timestamptz,
  overlap_expires_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz
);
CREATE INDEX agent_credentials_identity_idx ON public.agent_credentials(identity_id);
CREATE INDEX agent_credentials_family_idx ON public.agent_credentials(family_id,generation);

CREATE TABLE public.agent_enrollment_tokens (
  id uuid PRIMARY KEY,
  agent_id text NOT NULL,
  token_hash text NOT NULL UNIQUE CHECK (length(token_hash)=64),
  authorized_labels text[] NOT NULL DEFAULT '{}',
  authorized_capabilities text[] NOT NULL DEFAULT '{}',
  expires_at timestamptz NOT NULL,
  used_at timestamptz,
  revoked_at timestamptz,
  created_by text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE public.agent_enrollment_policies (
  id uuid DEFAULT gen_random_uuid() PRIMARY KEY,
  name text NOT NULL UNIQUE,
  provider text NOT NULL CHECK (provider='kubernetes'),
  provider_config jsonb NOT NULL DEFAULT '{}',
  subject_constraints jsonb NOT NULL DEFAULT '{}',
  agent_id_template text NOT NULL,
  allowed_labels text[] NOT NULL DEFAULT '{}',
  required_labels text[] NOT NULL DEFAULT '{}',
  authorized_capabilities text[] NOT NULL DEFAULT '{}',
  access_token_ttl interval NOT NULL DEFAULT interval '1 hour',
  enabled boolean NOT NULL DEFAULT true,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK (access_token_ttl >= interval '5 minutes' AND access_token_ttl <= interval '24 hours')
);
```

The down migration drops policies, enrollment tokens, credentials, external-subject index, then identities. Add `{13, "013_agent_identity_auth", "agent_credentials", "token_hash", ""}` to `schemaSentinels`.

- [ ] **Step 6: Verify migrations**

Run: `go test ./internal/store -run 'TestSchemaSentinelsCoverAllMigrations|TestPostgres_PATCreateAndGet' -count=1`

Expected: PASS; the integration test proves migration 013 applies to the shared template.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/agentauth internal/store/migrations/013_agent_identity_auth.* internal/store/verify.go
git commit -m "feat(auth): add agent credential primitives and schema"
```

---

### Task 2: PostgreSQL identity and credential repository

**Files:**
- Create: `internal/store/postgres_agent_auth.go`
- Create: `internal/store/postgres_agent_auth_test.go`
- Modify: `internal/store/store.go`

**Interfaces:**
- Produces: store models and transactional issue/rotation methods; the store accepts hashes only.

- [ ] **Step 1: Add store errors, models, and interface methods**

Add sentinel errors `ErrAgentCredentialNotFound`, `ErrAgentIdentityDisabled`, `ErrAgentEnrollmentInvalid`, and `ErrAgentRefreshReplay`. Add:

```go
type AgentIdentity struct {
	ID, AgentID, Status, EnrollmentMethod, ExternalSubject string
	AuthorizedLabels, AuthorizedCapabilities []string
	CreatedAt time.Time
	DisabledAt, LastAuthenticatedAt *time.Time
}

type AgentCredentialAuth struct {
	CredentialID, IdentityID, AgentID, Kind, TokenHash, Status string
	AuthorizedLabels, AuthorizedCapabilities []string
	ExpiresAt, CreatedAt time.Time
	RevokedAt *time.Time
}

type NewAgentCredential struct {
	ID, Kind, FamilyID, TokenHash string
	Generation int
	ExpiresAt time.Time
}

type AgentCredentialIssue struct {
	AgentID, EnrollmentMethod, ExternalSubject string
	AuthorizedLabels, AuthorizedCapabilities []string
	Access NewAgentCredential
	Refresh *NewAgentCredential
}

type AgentEnrollmentToken struct {
	ID, AgentID, CreatedBy string
	AuthorizedLabels, AuthorizedCapabilities []string
	ExpiresAt, CreatedAt time.Time
	UsedAt, RevokedAt *time.Time
}
```

Add these exact methods to `Store`:

```go
CreateAgentEnrollmentToken(context.Context, AgentEnrollmentToken, string) (*AgentEnrollmentToken, error)
ListAgentEnrollmentTokens(context.Context) ([]AgentEnrollmentToken, error)
RevokeAgentEnrollmentToken(context.Context, string) error
ConsumeAgentEnrollment(ctx context.Context, enrollmentID, presentedHash string, issue AgentCredentialIssue) (*AgentIdentity, error)
IssueExternalAgentAccess(ctx context.Context, issue AgentCredentialIssue) (*AgentIdentity, error)
GetAgentCredentialForAuth(context.Context, string) (*AgentCredentialAuth, error)
TouchAgentCredential(context.Context, string) error
RotateAgentRefresh(ctx context.Context, currentID, presentedHash string, now time.Time, access, refresh NewAgentCredential, overlap time.Duration) (*AgentIdentity, error)
SetAgentIdentityEnabled(context.Context, string, bool) error
RevokeAgentIdentityCredentials(context.Context, string) error
GetAgentIdentity(context.Context, string) (*AgentIdentity, error)
```

- [ ] **Step 2: Write failing integration tests**

Create tests named:

```go
func TestPostgres_ConsumeAgentEnrollmentIsSingleUse(t *testing.T)
func TestPostgres_CredentialAuthRejectsExpiredRevokedAndDisabled(t *testing.T)
func TestPostgres_ExternalIdentityIsStableAcrossReissue(t *testing.T)
func TestPostgres_RotateRefreshAllowsCrashOverlapRetry(t *testing.T)
func TestPostgres_RotateRefreshOutsideOverlapRevokesFamily(t *testing.T)
func TestPostgres_DeleteLiveAgentDoesNotDeleteIdentity(t *testing.T)
```

Race two consumers of one enrollment row; require one success and one `ErrAgentEnrollmentInvalid`. For refresh, issue family generation 1, rotate to 2, reuse generation 1 at `now+1m` and require generation 2 revoked/replaced, then reuse outside `now+5m` and require `ErrAgentRefreshReplay` plus family-wide revocation.

- [ ] **Step 3: Run red tests**

Run: `go test ./internal/store -run 'TestPostgres_(ConsumeAgentEnrollment|CredentialAuth|ExternalIdentity|RotateRefresh|DeleteLiveAgent)' -count=1`

Expected: FAIL with missing methods.

- [ ] **Step 4: Implement transactions**

`ConsumeAgentEnrollment` selects the enrollment row `FOR UPDATE`, constant-time compares hashes, checks unused/unrevoked/unexpired/fixed agent ID, creates or verifies an active compatible identity, inserts credentials, and marks used in one transaction. Disabled identities cannot be reactivated by enrollment.

`IssueExternalAgentAccess` upserts by `(enrollment_method, external_subject)`, rejects canonical-ID changes and disabled identities, updates policy-authorized labels/capabilities, and inserts access only.

`GetAgentCredentialForAuth` joins identity, maps missing/expired/revoked to `ErrAgentCredentialNotFound`, maps disabled to `ErrAgentIdentityDisabled`, and returns the stored hash for controller-side constant-time comparison.

`RotateAgentRefresh` locks refresh+identity and uses the injected `now`: first use supersedes old and inserts next generation; retry within overlap revokes the apparently lost next generation and issues another; use after overlap revokes the complete family, commits, and returns `ErrAgentRefreshReplay`.

- [ ] **Step 5: Run green tests**

Run: `go test ./internal/store -run 'TestPostgres_(ConsumeAgentEnrollment|CredentialAuth|ExternalIdentity|RotateRefresh|DeleteLiveAgent)' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/store.go internal/store/postgres_agent_auth.go internal/store/postgres_agent_auth_test.go
git commit -m "feat(store): persist per-agent identities and credentials"
```

---

### Task 3: AgentPrincipal middleware and route binding

**Files:**
- Create: `internal/controller/agent_auth.go`
- Create: `internal/controller/agent_auth_test.go`
- Modify: `internal/controller/server.go`
- Modify: `internal/controller/api_agent.go`
- Modify: `internal/controller/api_artifacts.go`
- Modify: `internal/controller/api_artifacts_test.go`

**Interfaces:**
- Produces: `AgentPrincipal`, `agentPrincipalFromContext`, `(*Server).agentAuth`, `requireAgentPathIdentity`, `(*Server).agentOrServerAuth`.

- [ ] **Step 1: Write failing auth and impersonation tests**

Cover valid principal attachment, wrong secret/expired/disabled rejection, A-token/B-path rejection, legacy-only-when-explicit, register body mismatch, claim label spoofing, and artifact upload by a non-owner. The label test sends `trusted:production` but stores only `pool:default`; assert `ClaimNextRun` receives only `pool:default`.

- [ ] **Step 2: Run red tests**

Run: `go test ./internal/controller -run 'TestAgentAuth|TestAgentRegister_RejectsBodyIdentityMismatch|TestAgentClaim_UsesAuthorizedPrincipalLabels|TestArtifactUpload_RejectsNonOwnerPrincipal' -count=1`

Expected: FAIL with missing principal/middleware.

- [ ] **Step 3: Implement principal authentication**

Use:

```go
type AgentPrincipal struct {
	IdentityID string
	AgentID string
	CredentialID string
	AuthMethod string
	AuthorizedLabels []string
	AuthorizedCapabilities []string
}
```

`agentAuth` accepts valid `uca_` only after DB lookup and constant-time hash comparison. Missing/wrong/expired/revoked/disabled returns generic `401 unauthorized`; other store errors return `503 authentication unavailable`. A non-`uca_` token may match only explicit `Config.LegacyAgentToken`, producing `AuthMethod:"legacy"`. Never fall back from an invalid `uca_` credential to legacy.

`requireAgentPathIdentity` skips only explicit legacy mode; otherwise compare to `chi.URLParam(r,"agentId")` and return `403 agent identity mismatch`. `agentOrServerAuth` attaches an agent principal or delegates to existing human `ServerAuth`.

- [ ] **Step 4: Rewire all routes and handlers**

Rename `Config.AgentToken` to `LegacyAgentToken`; remove `NewServer` fallback from human `Token`. Put `s.agentAuth` plus `requireAgentPathIdentity` on every `{agentId}` route, `s.agentAuth` on register/artifact upload, and `s.agentOrServerAuth` on artifact reads.

For non-legacy register/claim, use principal labels/capabilities, not request/query scheduling data, and do not synthesize a self-reported hostname label. Before upload, call `agentRunGuard(ctx, principal.AgentID, runID, false)`. Preserve current semantics only in explicit legacy mode.

Keep existing agent artifact read/list behavior behind `agentOrServerAuth`; do not silently narrow cross-run dependency downloads in this change. A capability-scoped artifact-read protocol requires its own design.

- [ ] **Step 5: Run green tests**

Run: `go test ./internal/controller -run 'TestAgentAuth|TestAgentRegister|TestAgentClaim|TestArtifactUpload' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/agent_auth.go internal/controller/agent_auth_test.go internal/controller/server.go internal/controller/api_agent.go internal/controller/api_artifacts.go internal/controller/api_artifacts_test.go
git commit -m "feat(controller): bind agent routes to authenticated identity"
```

---

### Task 4: Administrator enrollment and identity lifecycle APIs

**Files:**
- Create: `internal/api/agent_auth.go`
- Create: `internal/controller/api_agent_enrollment.go`
- Create: `internal/controller/api_agent_enrollment_test.go`
- Create: `internal/cli/agent_enrollment.go`
- Create: `internal/cli/agent_enrollment_test.go`
- Modify: `internal/controller/server.go`
- Modify: `internal/controller/audit.go`, `audit_test.go`
- Modify: `internal/cli/agent_install.go`

**Interfaces:**
- Produces: admin one-time enrollment CRUD, identity enable/disable/revoke, and matching CLI groups.

- [ ] **Step 1: Define DTOs and failing API tests**

Create JSON-tagged DTOs:

```go
type CreateAgentEnrollmentRequest struct {
	AgentID string `json:"agentId"`
	ExpiresIn string `json:"expiresIn,omitempty"`
	Labels []string `json:"labels,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}
type CreateAgentEnrollmentResponse struct {
	ID string `json:"id"`
	AgentID string `json:"agentId"`
	Token string `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type AgentEnrollmentMeta struct {
	ID string `json:"id"`
	AgentID string `json:"agentId"`
	CreatedBy string `json:"createdBy"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
	UsedAt *time.Time `json:"usedAt,omitempty"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
}
```

Also define secret-free `AgentIdentityMeta`. Tests cover admin-only create/revoke/enable/disable, viewer metadata reads, 10-minute default, invalid/non-positive duration, unknown capability, disabled identity not reactivated by issuing enrollment, and token present only in the creation response.

- [ ] **Step 2: Run red API tests**

Run: `go test ./internal/controller -run 'TestAgentEnrollmentAdmin|TestAgentIdentityAdmin' -count=1`

Expected: FAIL with missing DTOs/handlers.

- [ ] **Step 3: Implement admin routes**

Inside authenticated `/api/v1`, register:

```go
r.With(admin).Post("/agent-enrollments", s.handleCreateAgentEnrollment)
r.With(view).Get("/agent-enrollments", s.handleListAgentEnrollments)
r.With(admin).Delete("/agent-enrollments/{id}", s.handleRevokeAgentEnrollment)
r.With(view).Get("/agent-identities/{agentId}", s.handleGetAgentIdentity)
r.With(admin).Post("/agent-identities/{agentId}/enable", s.handleEnableAgentIdentity)
r.With(admin).Post("/agent-identities/{agentId}/disable", s.handleDisableAgentIdentity)
r.With(admin).Post("/agent-identities/{agentId}/credentials/revoke", s.handleRevokeAgentCredentials)
```

Creation generates `EnrollmentToken`, stores only its hash, validates a fixed agent ID/known capabilities, and returns plaintext once. Add explicit audit classifications `agent.enrollment.create`, `agent.enrollment.revoke`, `agent.identity.enable`, `agent.identity.disable`, and `agent.credentials.revoke`; resource is only enrollment UUID or canonical agent ID.

- [ ] **Step 4: Write failing CLI tests**

Test `agent enrollment create|list|revoke` and `agent identity get|enable|disable|revoke-credentials`. Assert admin bearer headers, no secret in URLs, one terminal display, no secret/hash in list output, and `--output-file` uses create-exclusive mode `0600` and refuses overwrite.

Run: `go test ./internal/cli -run 'TestAgentEnrollment|TestAgentIdentity' -count=1`

Expected: FAIL with missing command groups.

- [ ] **Step 5: Implement CLI commands**

Attach:

```go
cmd.AddCommand(newAgentEnrollmentCmd(resolve, httpClient))
cmd.AddCommand(newAgentIdentityCmd(resolve, httpClient))
```

Create flags: required `--agent-id`, default `--expires-in=10m`, repeatable `--label`, repeatable `--capability`, optional `--output-file`. Use `os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)`. Never accept a secret as a command-line flag.

- [ ] **Step 6: Run API/CLI/audit tests green**

Run: `go test ./internal/controller ./internal/cli -run 'AgentEnrollment|AgentIdentity|Audit' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/api/agent_auth.go internal/controller/api_agent_enrollment* internal/controller/server.go internal/controller/audit* internal/cli/agent_enrollment* internal/cli/agent_install.go
git commit -m "feat(auth): add administrative agent enrollment lifecycle"
```

---

### Task 5: One-time exchange, VM refresh, limiter, and telemetry

**Files:**
- Modify: `internal/api/agent_auth.go`
- Modify: `internal/controller/api_agent_enrollment.go`, `api_agent_enrollment_test.go`
- Create: `internal/controller/enrollment_limiter.go`, `enrollment_limiter_test.go`
- Modify: `internal/controller/server.go`
- Modify: `internal/metrics/metrics.go`, `metrics_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Produces: `POST /api/v1/agents/enroll` for `one-time-token`, `POST /api/v1/agents/token/refresh`, and bounded auth telemetry.

- [ ] **Step 1: Add response DTOs and failing exchange tests**

Add:

```go
type AgentEnrollRequest struct {
	Provider string `json:"provider"`
	Policy string `json:"policy,omitempty"`
}
type AgentTokenResponse struct {
	AgentID string `json:"agentId"`
	AccessToken string `json:"accessToken"`
	AccessExpiresAt time.Time `json:"accessExpiresAt"`
	RefreshToken string `json:"refreshToken,omitempty"`
	RefreshExpiresAt *time.Time `json:"refreshExpiresAt,omitempty"`
	Labels []string `json:"labels"`
	Capabilities []string `json:"capabilities"`
}
```

Test success, second-use/wrong/expired enrollment `401`, disabled `403`, access token rejected by refresh, refresh rotation, overlap retry, replay-family revocation, two controller instances sharing PostgreSQL, and concurrent single-use exchange.

- [ ] **Step 2: Run red tests**

Run: `go test ./internal/controller -run 'TestAgentEnroll|TestAgentRefresh|TestAgentEnrollmentHA' -count=1`

Expected: FAIL with missing bootstrap routes.

- [ ] **Step 3: Implement a bounded limiter**

Use the already-resolved `golang.org/x/time/rate` version. Key by provider + normalized remote IP + policy; never expose the key as a metric label. Hold at most 4096 LRU entries, burst 5, refill one request per 6 seconds. Inject time for tests. The sixth immediate request returns `429 enrollment rate limit exceeded`; refill succeeds and the map never exceeds 4096.

- [ ] **Step 4: Implement exchange and refresh**

Register both endpoints outside human `ServerAuth`, behind the limiter. Parse bearer values without logging them.

One-time exchange:

```text
parse uce -> generate family UUID + generation-1 access/refresh ->
ConsumeAgentEnrollment(hash only) -> return plaintext pair once
```

Use access 1h, refresh 30d. Map invalid/replayed/expired to generic `401 unauthorized`, disabled to `403 agent identity disabled`, DB failure to `503 enrollment unavailable`.

Refresh:

```text
parse ucr -> generate next access/refresh in same family ->
RotateAgentRefresh(hash, now, overlap=5m) -> return plaintext pair
```

Neither URL nor body accepts an agent ID. Do not add an access-token renewal endpoint.

- [ ] **Step 5: Add bounded metrics and security audit calls**

Add:

```text
unifiedcd_agent_auth_events_total{provider,result,reason}
unifiedcd_agent_legacy_auth_total
```

Provider folds to `one-time-token|kubernetes|access|refresh|other`; result to `success|failure`; reason to `ok|invalid|expired|disabled|policy|replay|rate_limited|unavailable|other`. Audit enrollment/refresh lifecycle events directly with non-secret resource IDs; do not audit routine heartbeat/claim traffic.

Also record principal/path, principal/body, and principal/run mismatch as bounded `reason=policy` security events. Throttle `TouchAgentCredential` to at most once per credential per five minutes using a bounded in-memory LRU; touch failure is observability-only and never changes an otherwise valid authorization decision.

- [ ] **Step 6: Run green/race tests**

Run: `go test -race ./internal/controller ./internal/metrics -run 'AgentEnroll|AgentRefresh|EnrollmentLimiter|AgentAuth' -count=1`

Expected: PASS without race reports.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/api/agent_auth.go internal/controller/api_agent_enrollment* internal/controller/enrollment_limiter* internal/controller/server.go internal/metrics
git commit -m "feat(auth): exchange and rotate short-lived agent tokens"
```

---

### Task 6: VM credential source and automatic startup enrollment

**Files:**
- Create: `internal/agent/token_source.go`
- Create: `internal/agent/credentials.go`, `credentials_test.go`
- Create: `internal/agent/credentials_unix.go`, `credentials_windows.go`
- Modify: `internal/agent/client.go`, `client_test.go`
- Modify: `internal/config/agent.go`, `internal/config/config_test.go`
- Modify: `cmd/agent/main.go`
- Modify: `internal/cli/agent_install.go`, `agent_install_test.go`

**Interfaces:**
- Produces: `TokenSource`, `NewClientWithTokenSource`, and concurrent-safe `CredentialManager`.
- Preserves: `NewClient(baseURL, token)` as explicit static/legacy constructor.

- [ ] **Step 1: Write failing tests**

Create tests named:

```go
func TestClientReadsTokenForEveryRequest(t *testing.T)
func TestCredentialManagerEnrollsAndPersistsRefreshOnly(t *testing.T)
func TestCredentialManagerRefreshesOnceForConcurrentCallers(t *testing.T)
func TestCredentialManagerRecoversAfterRestartWithRefresh(t *testing.T)
func TestCredentialManagerDoesNotUseAccessWhenPersistenceFails(t *testing.T)
func TestCredentialManagerRedactsTokenResponseErrors(t *testing.T)
func TestCredentialFileRejectsLooseUnixPermissions(t *testing.T)
```

Twenty concurrent `Token` calls must cause one exchange/refresh. A forced persistence failure must prevent the new access token from being returned.

- [ ] **Step 2: Run red tests**

Run: `go test ./internal/agent -run 'TestClientReadsToken|TestCredentialManager|TestCredentialFile' -count=1`

Expected: FAIL with missing types.

- [ ] **Step 3: Introduce TokenSource**

```go
type TokenSource interface { Token(context.Context) (string, error) }
func NewClientWithTokenSource(baseURL string, source TokenSource, httpClient *http.Client) *Client
```

`NewClient` wraps a static source. Every request obtains a token immediately before setting Authorization. Errors never include bearer values or response bodies containing token fields.

- [ ] **Step 4: Implement CredentialManager and persistence**

The manager owns server, agent ID, enrollment-token path, refresh-file path, access token/expiry, clock, jitter source, and mutex. Return cached access only while more than `15m+jitter` remains; otherwise serialize exchange/refresh.

Persist only:

```json
{"version":1,"agentId":"vm-agent-01","refreshToken":"ucr_example","refreshExpiresAt":"2030-01-01T00:00:00Z"}
```

Write a same-directory temporary file, sync, protect/validate it, atomically rename, and sync the directory on Unix. Unix requires no group/world bits. Windows validates that the configured credential directory's inherited DACL does not grant write access to broad principals; failure text is `credential directory ACL is not restricted`. Never persist access.

- [ ] **Step 5: Add startup modes and installer output**

Add config/env/flags:

```go
CredentialFile string `yaml:"credentialFile"`          // UNIFIED_AGENT_CREDENTIAL_FILE
EnrollmentTokenFile string `yaml:"enrollmentTokenFile"` // UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE
```

Precedence: explicit legacy token (warn), existing credential file, enrollment-token file, otherwise `agent credentials are required`. Generated systemd/launchd units contain file paths, never `--token=<secret>`.

- [ ] **Step 6: Run green tests**

Run: `go test -race ./internal/agent ./internal/config ./internal/cli -run 'Token|Credential|AgentInstall' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/agent/token_source.go internal/agent/credentials* internal/agent/client* internal/config/agent.go internal/config/config_test.go cmd/agent/main.go internal/cli/agent_install*
git commit -m "feat(agent): automate VM enrollment and credential rotation"
```

---

### Task 7: Enrollment policies and Kubernetes identity verification

**Files:**
- Modify: `internal/store/store.go`, `postgres_agent_auth.go`, `postgres_agent_auth_test.go`
- Modify: `internal/api/agent_auth.go`
- Create: `internal/controller/agent_enrollment_kubernetes.go`, `agent_enrollment_kubernetes_test.go`
- Modify: `internal/controller/api_agent_enrollment.go`, `api_agent_enrollment_test.go`, `server.go`
- Modify: `internal/config/controller.go`, `internal/config/config_test.go`
- Modify: `cmd/controller/main.go`
- Modify: `internal/cli/agent_enrollment.go`, `agent_enrollment_test.go`

**Interfaces:**
- Produces: policy CRUD, configured cluster verifiers, and `provider=kubernetes` exchange.

- [ ] **Step 1: Add policy store tests and methods**

Add the exact store model and CRUD:

```go
type AgentEnrollmentPolicy struct {
	ID string
	Name string
	Provider string
	ProviderConfig json.RawMessage
	SubjectConstraints json.RawMessage
	AgentIDTemplate string
	AllowedLabels []string
	RequiredLabels []string
	AuthorizedCapabilities []string
	AccessTokenTTL time.Duration
	Enabled bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

UpsertAgentEnrollmentPolicy(context.Context, AgentEnrollmentPolicy) (*AgentEnrollmentPolicy, error)
GetAgentEnrollmentPolicy(context.Context, string) (*AgentEnrollmentPolicy, error)
ListAgentEnrollmentPolicies(context.Context) ([]AgentEnrollmentPolicy, error)
DeleteAgentEnrollmentPolicy(context.Context, string) error
```

The Kubernetes JSON shapes are `{"cluster":"<configured-key>"}` and `{"namespaces":["unified-cd"],"serviceAccounts":["unified-cd-k8s-agent"]}`. The only accepted initial template is `k8s:{cluster}:{namespace}:{podUID}`; reject unknown placeholders instead of implementing a general template language. Tests cover JSON round-trip, deterministic name order, update timestamp, enabled state, and 5m–24h TTL bounds.

- [ ] **Step 2: Add controller cluster configuration tests/types**

```go
type ControllerAgentAuthConfig struct {
	LegacySharedToken string `yaml:"legacySharedToken"`
	KubernetesClusters []ControllerKubernetesClusterConfig `yaml:"kubernetesClusters"`
}
type ControllerKubernetesClusterConfig struct {
	Name string `yaml:"name"`
	Kubeconfig string `yaml:"kubeconfig"`
}
```

Add `AgentAuth *ControllerAgentAuthConfig` to controller config. `UNIFIED_AGENT_LEGACY_TOKEN` fills only legacy shared token. Multiple clusters are YAML-only; reject empty/duplicate names and more than one empty kubeconfig.
`ControllerEffective` always returns a non-nil `AgentAuth` value so `cmd/controller` can read it without a nil branch; an empty value means secure per-agent-only mode.

- [ ] **Step 3: Write failing Kubernetes verifier tests**

With `kubernetes/fake`, cover valid bound Pod, wrong audience, ServiceAccount/namespace policy mismatch, Pod UID mismatch, deleted Pod, requested-label escalation, and same Pod UID reissuing into the same identity. Canonical ID is `k8s:<cluster>:<namespace>:<pod-uid>`.

- [ ] **Step 4: Implement TokenReview and Pod binding**

Define:

```go
type KubernetesEnrollmentVerifier interface {
	Verify(context.Context, string, store.AgentEnrollmentPolicy) (KubernetesEnrollmentIdentity, error)
}
type KubernetesEnrollmentIdentity struct {
	Cluster, Namespace, ServiceAccount, PodName, PodUID string
}
```

Submit `TokenReview` with the required audience, require authenticated status/returned audience, parse the already-validated projected-token payload for bound Pod claims, fetch the Pod, then compare namespace/name/UID/ServiceAccount. Reject policies without both namespace and ServiceAccount constraints. Controller cluster credentials are configured secret references, never policy JSON.

- [ ] **Step 5: Add policy API/CLI and Kubernetes exchange**

Add API DTOs whose JSON contract is:

```go
type AgentEnrollmentPolicyRequest struct {
	Provider string `json:"provider"`
	Cluster string `json:"cluster"`
	Namespaces []string `json:"namespaces"`
	ServiceAccounts []string `json:"serviceAccounts"`
	AgentIDTemplate string `json:"agentIdTemplate"`
	AllowedLabels []string `json:"allowedLabels"`
	RequiredLabels []string `json:"requiredLabels"`
	Capabilities []string `json:"capabilities"`
	AccessTokenTTL string `json:"accessTokenTTL"`
	Enabled bool `json:"enabled"`
}
```

Add admin `POST /api/v1/agent-enrollment-policies` for create, admin `PUT /api/v1/agent-enrollment-policies/{name}` for update, viewer GET/list, and admin delete. CLI group `agent enrollment-policy create|update|get|list|delete` uses flags for cluster, repeated namespace/ServiceAccount, ID template, allowed/required labels, capabilities, TTL, and enabled state; it never accepts kubeconfig contents.

Classify create/update/delete in `auditActionTable` as `agent.policy.create`, `agent.policy.update`, and `agent.policy.delete`; audit only the policy name, never cluster credentials or ServiceAccount JWTs.

For `provider=kubernetes`, load enabled policy, select its configured verifier, validate the bearer ServiceAccount token, derive identity/authority, generate access only using policy TTL, and call `IssueExternalAgentAccess`. Return `403 enrollment policy rejected` for policy mismatch and `503 kubernetes identity unavailable` for cluster API failures.

- [ ] **Step 6: Run green tests**

Run: `go test ./internal/store ./internal/controller ./internal/config ./internal/cli -run 'EnrollmentPolicy|KubernetesEnrollment|ControllerAgentAuth' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/store internal/api/agent_auth.go internal/controller/agent_enrollment_kubernetes* internal/controller/api_agent_enrollment* internal/controller/server.go internal/config/controller.go internal/config/config_test.go cmd/controller/main.go internal/cli/agent_enrollment*
git commit -m "feat(auth): verify Kubernetes workload identity for enrollment"
```

---

### Task 8: Kubernetes agent automatic enrollment and manifests

**Files:**
- Create: `internal/k8sagent/credentials.go`, `credentials_test.go`
- Modify: `internal/k8sagent/config.go`, `config_test.go`
- Modify: `cmd/k8s-agent/main.go`
- Modify: `manifests/base/k8s-agent/config-configmap.yaml`, `config-secret.yaml`, `deployment.yaml`, `kustomization.yaml`
- Create/Modify: `manifests/base/controller/serviceaccount.yaml`, `rbac.yaml`, `deployment.yaml`, `kustomization.yaml`
- Modify: generated aggregate manifests under `manifests/`

**Interfaces:**
- Produces: projected-token `TokenSource`; consumes Task 7 exchange and uses controller-returned canonical agent ID.

- [ ] **Step 1: Write failing credential-source tests**

Test initial exchange, cached access, projected JWT reread after file replacement, one exchange for concurrent callers, retryable `503`, response AgentID overriding configured Pod name, and absence of refresh-token persistence.

- [ ] **Step 2: Run red tests**

Run: `go test ./internal/k8sagent -run 'TestKubernetesCredential' -count=1`

Expected: FAIL with missing source/config.

- [ ] **Step 3: Implement source and config**

Add:

```go
EnrollmentPolicy string `yaml:"enrollmentPolicy"`
ServiceAccountTokenFile string `yaml:"serviceAccountTokenFile"`
```

Default the token file to `/var/run/secrets/unified-cd-agent/token`. Per-agent mode does not require configured `Token` or `AgentID`; first exchange supplies ID and authoritative labels. Preserve both only with explicit legacy token. Reread the projected file for every replacement so kubelet rotation is observed, then build `agentlib.NewClientWithTokenSource`.

- [ ] **Step 4: Update secure-default manifests**

Project:

```yaml
volumes:
  - name: enrollment-identity
    projected:
      sources:
        - serviceAccountToken:
            path: token
            audience: unified-cd-agent-enrollment
            expirationSeconds: 3600
```

Mount read-only at `/var/run/secrets/unified-cd-agent`. Remove the shared agent token Secret from default agent Deployment/kustomization. Give the controller ServiceAccount only `create` on `tokenreviews.authentication.k8s.io` plus `get` on agent Pods in the configured namespace; never grant cluster-admin. Keep existing run-Pod RBAC unchanged.

- [ ] **Step 5: Regenerate and validate manifests**

Use the generation command documented in `manifests/README.md`, not manual copying. Then:

```bash
kubectl kustomize manifests/base > /tmp/unified-cd-rendered.yaml
kubectl apply --dry-run=client -f /tmp/unified-cd-rendered.yaml
```

Expected: success; rendered agent Deployment contains the audience and no shared `CHANGEME` token.

- [ ] **Step 6: Run green tests**

Run: `go test ./internal/k8sagent ./cmd/k8s-agent -count=1`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/k8sagent/credentials* internal/k8sagent/config.go internal/k8sagent/config_test.go cmd/k8s-agent/main.go manifests
git commit -m "feat(k8s): enroll each agent pod with service account identity"
```

---

### Task 9: Secure default, explicit legacy migration, and route matrix

**Files:**
- Modify: `internal/config/controller.go`, `internal/config/config_test.go`
- Modify: `cmd/controller/main.go`
- Modify: `internal/controller/server.go` and tests using `Config{AgentToken: ...}`
- Create: `internal/controller/agent_auth_routes_test.go`
- Modify: `.env.example`

**Interfaces:**
- Produces: secure new-install default and observable opt-in compatibility mode.
- Verifies: every agent route rejects A acting as B.

- [ ] **Step 1: Write failing configuration/startup tests**

Pin these results:

```text
UNIFIED_TOKEN alone                 => human PAT only
UNIFIED_AGENT_LEGACY_TOKEN set      => explicit compatibility token
agentAuth.legacySharedToken in YAML => overrides environment
empty compatibility setting        => uca credentials only
legacy request                      => counter increment and startup warning
```

Rename `AgentToken` test literals to `LegacyAgentToken`; do not reintroduce fallback to make tests pass.

- [ ] **Step 2: Add the route impersonation matrix**

Create identities A/B and a run claimed by B. Exercise every `/api/v1/agents/{agentId}` method and artifact upload with A's access token and B's path/run; require `403`. Cover register body mismatch separately. Keep the route list beside route registration so future endpoints must update this test.

- [ ] **Step 3: Run red tests**

Run: `go test ./internal/config ./internal/controller ./cmd/controller -run 'LegacyAgent|AgentRouteIdentityMatrix' -count=1`

Expected: FAIL until secure-default wiring is complete.

- [ ] **Step 4: Complete startup wiring**

Pass `LegacyAgentToken: eff.AgentAuth.LegacySharedToken`. Remove current `AgentToken: *token`. Warn once when compatibility is non-empty. `UNIFIED_AGENT_TOKEN` remains only the agent binary's legacy input; the controller uses deliberately distinct `UNIFIED_AGENT_LEGACY_TOKEN`.

Update `.env.example`: secure defaults contain no shared runtime agent token; compatibility appears only in a migration-labeled block.

- [ ] **Step 5: Run green tests**

Run: `go test ./internal/config ./internal/controller ./cmd/controller -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config/controller.go internal/config/config_test.go cmd/controller/main.go internal/controller .env.example
git commit -m "security: make shared agent authentication explicit and temporary"
```

---

### Task 10: Documentation, migration guide, and full verification

**Files:**
- Create: `docs/migration-agent-auth.md`
- Modify: `docs/authentication.md`, `authorization.md`, `agents.md`, `configuration.md`, `kubernetes-integration.md`, `operations.md`, `troubleshooting.md`, `audit.md`, `cli.md`, `getting-started.md`
- Modify: `manifests/README.md`, `README.md`, `.env.example`
- Modify: affected `examples/config/**`, `templates/**`, and READMEs found by the stale-token sweep
- Modify generated artifacts only if `go generate ./...` changes them

**Interfaces:**
- Documents and verifies the exact behavior delivered by Tasks 1–9.

- [ ] **Step 1: Write rollout, rollback, and failure documentation**

`docs/migration-agent-auth.md` uses this order:

```text
1. Upgrade controller with UNIFIED_AGENT_LEGACY_TOKEN temporarily set.
2. Create VM enrollments and restart VM agents with credential files.
3. Create Kubernetes policy and roll the agent Deployment with projected tokens.
4. Confirm unifiedcd_agent_legacy_auth_total does not increase for one rollout window.
5. Remove UNIFIED_AGENT_LEGACY_TOKEN and the old Secret.
6. Revoke leftover enrollment tokens and inspect active identities.
```

Include rollback, lost VM refresh recovery, disabled identity, expired enrollment, TokenReview `503`, policy rejection, refresh replay, database outage, and exact grep-able error strings. State that root Compose is development-only and production requires HTTPS. Describe optional mTLS as future work, never as implemented.

- [ ] **Step 2: Update all user-facing references**

Document commands, API paths, YAML/env settings, TTLs/bounds, token prefixes, one-time display, label/capability authority, audit actions, and metrics. Kubernetes examples use projected ServiceAccount identity. VM examples use token/credential file paths and never put secrets in process arguments.

- [ ] **Step 3: Run stale-token and secret-output sweeps**

Run:

```bash
rg -n "AgentToken: \*token|same-token-as-UNIFIED_TOKEN|token: your-agent-token|token: my-agent-token|CHANGEME" docs examples templates manifests README.md .env.example cmd internal
rg -n "Authorization.*(slog|Printf|Errorf)|accessToken.*(slog|Printf)|refreshToken.*(slog|Printf)" cmd internal
```

Expected: zero insecure runtime examples. Migration-guide hits must be clearly marked legacy and contain no real secret.

- [ ] **Step 4: Run focused and race tests**

Run:

```bash
go test ./internal/agentauth ./internal/store ./internal/controller ./internal/agent ./internal/k8sagent ./internal/cli ./internal/config ./internal/metrics -count=1
go test -race ./internal/controller ./internal/agent ./internal/k8sagent -count=1
```

Expected: PASS without race reports.

- [ ] **Step 5: Run generation, parsing gates, full tests, and builds**

Run:

```bash
go generate ./...
go test ./internal/dsl -run 'Templates|Examples' -count=1
go test ./... -count=1
go build ./cmd/...
git diff --check
```

Expected: PASS. Review and commit generated diffs; never hand-edit generated `docs/field-reference.md` or `schemas/unified-cd.schema.json`.

- [ ] **Step 6: Manual two-agent security smoke test**

Enroll A/B. Send A's access token to B's heartbeat and claim paths; expect `403 agent identity mismatch`. Disable A; expect A `401` while B continues. Restart a controller replica between enrollment and VM refresh; expect refresh success from PostgreSQL state.

- [ ] **Step 7: Commit**

```bash
git add README.md docs examples templates manifests schemas .env.example
git commit -m "docs: document per-agent enrollment and shared-token migration"
```

---

## Completion Gate

- Two normal-mode agents have different runtime credentials.
- A cannot authenticate as B on any route; disabling A does not interrupt B.
- A Kubernetes Pod enrolls unattended with a bound ServiceAccount token and receives no refresh credential.
- A VM restarts after access expiry through its scoped refresh credential.
- Access tokens cannot self-renew; PostgreSQL contains hashes only.
- Two controller replicas issue and validate consistently.
- Legacy mode is explicit, observable, and removable.
- Handlers consume transport-neutral `AgentPrincipal`, preserving a clean future mTLS path.
