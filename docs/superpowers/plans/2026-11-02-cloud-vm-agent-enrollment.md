# Automated Host-Agent Enrollment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enrollment-only PATs (`agent.enroll` with a label bound) and AWS/GCP/Azure VM identity enrollment for the host agent.

**Architecture:** Part 1 adds a global-only permission and a per-PAT label bound enforced in `handleCreateAgentEnrollment`. Part 2 generalises enrollment policies beyond `kubernetes`, adds one verifier per cloud behind a common interface used by `POST /api/v1/agents/enroll`, and gives the host agent an access-token-only credential source (modelled on `internal/k8sagent/credentials.go`) that re-proves its cloud identity before expiry.

**Tech Stack:** Go 1.26, chi, pgx + golang-migrate, `github.com/coreos/go-oidc/v3` (GCP/Azure JWT), `github.com/go-jose/go-jose/v4` (test JWKS), `github.com/minio/minio-go/v7/pkg/signer` (SigV4) or a hand-written SigV4, cobra CLI.

**Spec:** `docs/superpowers/specs/2026-11-02-cloud-vm-agent-enrollment-design.md`. Code map with file:line for everything referenced below: `C:\Users\arima\AppData\Local\Temp\claude\C--Users-arima-unified-cd-project\072b4471-dc5d-4ea2-ad3d-e1b2cd4d3a4a\scratchpad\enroll-code-map.md` (read the sections your task names).

## Global Constraints

- AGENTS.md: English only; no PII (examples use `example@example.com`, account `123456789012`); docs, `examples/` and `templates/` updated in the same task as the behaviour.
- Additive only: nothing changes for a setup that does not use `agent.enroll`, `--enroll-label`, or a non-`kubernetes` provider. The k8s enroll request/response JSON stays byte-compatible.
- The controller holds no AWS/GCP/Azure credentials and calls no cloud API except AWS STS `GetCallerIdentity` (forwarding the agent's signed request) and public JWKS endpoints.
- Errors to callers of `POST /api/v1/agents/enroll` for cloud providers: `403 enrollment policy rejected` (any verification or policy mismatch, unknown provider), `503 <provider> identity unavailable` (JWKS/STS unreachable or 5xx), `403 agent identity disabled` (existing `respondAgentCredentialError`). Details go to the controller log (`slog`) and audit only.
- Docs must say each cloud provider was verified only against simulated responses in this release.
- Commits: `git -c user.name=eirueimi -c user.email="128969149+eirueimi@users.noreply.github.com" commit`, conventional subject, body ends with the two trailer lines `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_01G3NWwwXnjYtSfzJjZYTrnV`. No `-s`.
- Docs build: `"C:\Users\arima\AppData\Local\Temp\claude\C--Users-arima-unified-cd-project\072b4471-dc5d-4ea2-ad3d-e1b2cd4d3a4a\scratchpad\mkdocs-venv\Scripts\python" -m mkdocs build --strict -d <new dir under the scratchpad>` from the worktree root, whenever a task touches `docs/` or `mkdocs.yml`.
- Controller/store tests need Docker (dockertest Postgres); a "postgres not ready" or timeout failure is contention: re-run that package alone before concluding.

## Rulings made while planning (deviations from or refinements of the spec)

1. **Role-only creators and `agent.enroll`.** Today a role-only creator (break-glass `UNIFIED_TOKEN`, role-only PAT) cannot pass `permissions`, so a non-SSO install could never mint an enrollment-only PAT. Ruling: a role-only creator may create an *ownerless* PAT whose ceiling is exactly `["agent.enroll"]` (plus `--enroll-label`). `patPrincipal` grants such a PAT exactly `agent.enroll` globally. Any other permission list from a role-only creator keeps the existing 400.
2. **Creator check.** `validateCeiling` treats `agent.enroll` as held when the creator holds `agent.enroll` or `agent.manage`.
3. **AWS STS host.** Allowed hosts are `sts.amazonaws.com` and `sts.<region>.amazonaws.com` where `<region>` matches `^[a-z]{2}(-[a-z]+)+-[0-9]$`; if the policy's `stsRegions` is non-empty the region must be in it. `stsRegions` is optional (the spec listed it in ProviderConfig without saying required).
4. **Azure.** Plain VMs with a system-assigned identity only: `xms_mirid` must be `/subscriptions/<sub>/resourcegroups/<rg>/providers/Microsoft.Compute/virtualMachines/<name>` (case-insensitive). VM scale sets (their system identity is per scale set, not per instance) and user-assigned identities are rejected and listed in TODO. Agent ID `azure:<subscription>:<resourcegroup>:<vmname>`, lower-cased.
5. **Audience on the agent.** The agent is told the audience with a new setting (`--enrollment-audience`); it must equal the policy's `audience`.
6. **Cloud policies are API/CLI only.** The controller config file keeps defining only kubernetes policies (`kubernetesEnrollmentPolicies`); no config-file form for cloud policies in this release.
7. **End-to-end test** is an in-process test (real `NewServer` + real agent credential source + simulated GCP JWKS), not a `test/e2e` binary test, because the controller binary cannot be pointed at a simulated issuer.

## Review Focus

1. Azure managed-identity tokens may be v1 (`iss = https://sts.windows.net/<tid>/`) or v2 (`https://login.microsoftonline.com/<tid>/v2.0`): both must verify for the policy's tenant, nothing else. Test in Task 7.
2. Azure IDs differ in case between sources (`resourceGroups` vs `resourcegroups`, upper-case RG names): subscription and resource-group matching is case-insensitive; the agent ID is lower-cased. Test in Task 7.
3. A host agent started with both a cloud provider and an enrollment token / `--id` / existing credential flags must fail at startup with a clear message, not silently pick one. Test in Task 9.
4. Removing a label from a policy must take effect at the agent's next re-proof (labels are refreshed on every exchange). Test in Task 5.
5. `--enroll-label` with blank or duplicate entries, and a PAT-from-PAT trying to widen the parent's bound, must be rejected with 400. Test in Task 2.

---

### Task 1: `agent.enroll` permission

**Files:**
- Modify: `internal/authz/catalogue.go`, `internal/controller/authz_routes.go:138`
- Test: `internal/authz/catalogue_test.go`, `internal/authz/describe_test.go:107`, `internal/controller/api_authz_test.go:227`, `internal/controller/authz_routes_test.go`
- Docs: every "25 permissions" / "Sixteen" count listed in code map section H (`authorization.md:3,11,232,415`, `configuration.md:166`, `troubleshooting/controller-and-database.md:29,65`, `contributing/auth-implementation.md:37`), plus the catalogue row and route-table row in `authorization.md`.

**Interfaces:**
- Produces: `authz.AgentEnroll Permission = "agent.enroll"`, global-only, appended at the end of the global-only block and of `catalogue` (index 25) so existing bit positions do not move; in the `admin` bundle only (via `AllSet`).

- [ ] **Step 1:** Change `TestCatalogueHas25PermissionsInOrder` to `TestCatalogueHas26PermissionsInOrder`: `require.Len(t, all, 26)`, `require.Equal(t, authz.AgentEnroll, all[25])`, still 9 folder-scopable; `assert.False(t, authz.FolderScopable(authz.AgentEnroll))`; `Bundle("developer")` and `Bundle("viewer")` do not contain it; `Bundle("admin")` does. Update the two other 25-count assertions to 26. Add a route test: a caller holding only `agent.enroll` passes the authorize step for `POST /api/v1/agent-enrollments` and gets `403 forbidden: missing permission "agent.enroll"`-style refusal on `DELETE /api/v1/agent-enrollments/{id}` and `POST /api/v1/agent-enrollment-policies`.
- [ ] **Step 2:** Run `go test ./internal/authz/ ./internal/controller/ -run 'Catalogue|Bundle|Describe|Authz|Route' -count=1`; expect the count and route tests to fail.
- [ ] **Step 3:** Add the constant and catalogue entry; change the route rule to `{Method: mPost, Pattern: "/api/v1/agent-enrollments", Perm: authz.AgentManage, AltPerm: authz.AgentEnroll, Scope: scopeGlobal}` (shape of the tokens rules at `authz_routes.go:135`). Update the comment "positions 9-24".
- [ ] **Step 4:** Re-run; also `go test ./internal/controller/ -run 'Golden|Matrix' -count=1` (golden/matrix files may need the new permission; regenerate only through their documented mechanism).
- [ ] **Step 5:** Update the docs counts (26, seventeen global-only) and add the `agent.enroll` row: "Create host-agent enrollment tokens, limited to the labels bound to the caller's PAT (see Enrollment-only PATs)". mkdocs build. Commit `feat(authz): add agent.enroll permission`.

### Task 2: PAT enrollment label bound

**Files:**
- Create: `internal/store/migrations/025_pat_enroll_labels.up.sql` / `.down.sql` (`ALTER TABLE pats ADD COLUMN enroll_labels text[]`, NULL = none)
- Modify: `internal/store/store.go` (`PAT.EnrollLabels []string`, `NewOwnedPAT.EnrollLabels []string`, new `NewEnrollPAT`), `internal/store/postgres.go` (`patColumns`, `scanPAT`, `CreateOwnedPAT`, new `CreateEnrollPAT`), store interface, `internal/api/types.go` (`CreatePATRequest.EnrollLabels []string json:"enrollLabels,omitempty"`, same field on `CreatePATResponse` and `PATMeta`), `internal/controller/api_tokens.go` (`handleCreateToken`, `validateCeiling`, `handleListTokens`), `internal/controller/auth.go` (`Principal.EnrollLabels []string`, set in `patPrincipal`; ruling 1 branch), `internal/cli/token.go` (`--enroll-label` StringArray, list output, old-controller guard).
- Test: `internal/store/postgres_*_test.go` (migration + round trip), `internal/controller/api_tokens_role_test.go`, `api_tokens_owner_test.go`, `internal/cli/token_test.go`.

**Interfaces:**
- Produces: `Principal.EnrollLabels []string` (nil = no bound) for PAT callers; `func (p *Postgres) CreateEnrollPAT(ctx context.Context, in store.NewEnrollPAT) (*store.PAT, error)` with `type NewEnrollPAT struct { Name, TokenHash string; EnrollLabels []string; ExpiresAt *time.Time }` storing `role = ""`, `permissions = {agent.enroll}`, no owner.

- [ ] **Step 1: Failing tests.**
  - Store: migration 025 applies on top of 024 (`MigrateTo` pattern of `TestMigration024DropsRoleDefaults`); `CreateOwnedPAT` with `EnrollLabels: []string{"kind:linux"}` round-trips through `GetPATByHash` and `ListPATs`; `CreateEnrollPAT` round-trips with `Owner == nil`, `Permissions == []string{"agent.enroll"}`.
  - Controller: owner creator holding `agent.manage` creates `{"name":"prov","permissions":["agent.enroll"],"enrollLabels":["kind:linux","pool:build"]}` → 200 and response echoes `enrollLabels`; break-glass creator (`"secret"`) creates the same → 200 (ruling 1); break-glass with `["agent.enroll","job.read"]` → 400 `cannot grant permissions: this token has no owner; use --role`; `enrollLabels` without `agent.enroll` in permissions → 400 `enrollLabels requires the agent.enroll permission`; blank entry → 400 `enrollLabels must not contain empty entries`; duplicate → 400 `enrollLabels must not contain duplicates`; creator holding neither `agent.enroll` nor `agent.manage` → 400 `cannot grant permission "agent.enroll": you do not hold it`; PAT-from-PAT whose parent bound is `["kind:linux"]` asking for `["pool:build"]` → 400 `enrollLabels must be a subset of the creating token's enrollLabels`; `GET /api/v1/tokens` lists `enrollLabels`.
  - Principal: a PAT created by ruling 1 authenticates with `Grants.Can(authz.AgentEnroll, "")` true and `Grants.Can(authz.AgentRead, "")` false, `EnrollLabels` set.
  - CLI: `token create prov --permission agent.enroll --enroll-label kind:linux` sends `enrollLabels`; prints `Enroll labels: kind:linux`; if the response lacks `enrollLabels` it revokes the token and errors `the server does not support --enroll-label`; `token list` shows the labels.
- [ ] **Step 2:** Run `go test ./internal/store/ ./internal/controller/ ./internal/cli/ -run 'PAT|Token|Migration' -count=1` and see them fail.
- [ ] **Step 3:** Implement. `validateCeiling(creator, role, perms)` gains: for `agent.enroll`, held if `creator.Grants.HeldAnywhere(authz.AgentEnroll) || creator.Grants.HeldAnywhere(authz.AgentManage)`. Ruling 1 branch in the role-only path: `req.Permissions` deep-equals `["agent.enroll"]` and `req.Role == ""` → `CreateEnrollPAT`. `patPrincipal`: `pat.Owner == nil && pat.Role == "" && pat.Permissions == ["agent.enroll"]` → `Grants` with exactly `agent.enroll` global (grantedBy `pat:<name>`), `EnrollLabels: pat.EnrollLabels`; owner PATs also copy `EnrollLabels`.
- [ ] **Step 4:** Re-run the tests; `go vet ./...`.
- [ ] **Step 5:** Docs: `reference/cli.md` token create usage line/flag table/example, `troubleshooting/controller-and-database.md` token-refusal table (each new string), `authorization.md` PAT section (enroll-label bound; ruling 1). Commit `feat(auth): enrollment label bound on PATs`.

### Task 3: Enforce the bound on enrollment-token creation

**Files:**
- Modify: `internal/controller/api_agent_enrollment.go` (`handleCreateAgentEnrollment`), `internal/controller/audit.go` (resource for `agent.enrollment.create`), `internal/cli/agent_enrollment.go` (print the response body on a non-201)
- Test: `internal/controller/api_agent_enrollment_test.go`, `internal/cli/agent_enrollment_test.go`
- Docs: `docs/operator-manual/agents.md` (new subsection "Provisioning many VMs: an enrollment-only PAT", replacing any advice to give a provisioner `agent.manage`), `docs/reference/cli.md` enrollment create, `docs/troubleshooting/agents-and-enrollment.md` (new strings), `docs/operator-manual/audit.md` (resource now carries labels).

**Interfaces:**
- Consumes: `authz.AgentEnroll` (Task 1), `Principal.EnrollLabels` (Task 2).

- [ ] **Step 1: Failing tests** (caller "enroll-only" = holds `agent.enroll`, not `agent.manage`):
  - labels `["kind:linux"]` within bound `["kind:linux","pool:build"]` → 201;
  - label `pool:gpu` outside bound → 403 body `label "pool:gpu" is not in this token's enrollment labels`;
  - no bound and labels `["kind:linux"]` → 403 same string; no bound and no labels → 201;
  - explicit `agentId` (even a valid one) → 403 `agentId cannot be set with agent.enroll; it is generated`;
  - `agent.manage` caller: unchanged (explicit ID and any label allowed);
  - audit entry for the create has `Resource` = the created agent ID and the labels, e.g. `agent-1a2b3c4d labels=kind:linux,pool:build`;
  - a cloud-shaped ID `aws:123456789012:i-0abc12345678` from an `agent.manage` caller → 400 (existing `agentid.PortableError`) — pins spec "cloud-derived ID cannot be requested";
  - CLI: on 403 the command error includes the server's body text.
- [ ] **Step 2:** Run `go test ./internal/controller/ ./internal/cli/ -run 'AgentEnrollment' -count=1`; fail.
- [ ] **Step 3:** Implement: after JSON decode and before the ID default, if `!p.Grants.Can(authz.AgentManage, "")` apply the two checks on the raw request. Audit resource: set it the way the middleware supports for other routes (`auditResourceParams` or an explicit context value); add the hook if none exists.
- [ ] **Step 4:** Re-run; mkdocs build after docs.
- [ ] **Step 5:** Commit `feat(agent): enrollment-only callers are bound to their PAT's labels`.

### Task 4: Generalise enrollment policies to aws/gcp/azure

**Files:**
- Create: `internal/store/migrations/026_enrollment_policy_providers.up.sql` / `.down.sql` (drop the `provider='kubernetes'` CHECK from migration 013 by looking it up in `pg_constraint`, the way `014_*.up.sql` does; add `CHECK (provider IN ('kubernetes','aws','gcp','azure'))`; down restores the kubernetes-only CHECK after deleting non-kubernetes rows)
- Modify: `internal/store/postgres_agent_auth.go` (`validateAgentEnrollmentPolicy`), `internal/api/agent_auth.go` (`AgentEnrollmentPolicyRequest`), `internal/controller/api_agent_enrollment.go` (`enrollmentPolicyFromRequest`, `enrollmentPolicyResponse`), `internal/cli/agent_enrollment.go` (policy write command), `internal/metrics/metrics.go` (`agentAuthProvider`)
- Test: `internal/store/postgres_agent_auth_test.go`, `internal/controller/api_agent_enrollment_test.go`, `internal/cli/agent_enrollment_test.go`, `internal/metrics/metrics_test.go:79`

**Interfaces:**
- Produces (store JSON shapes, used by Tasks 6-8):
  - aws: `ProviderConfig {"audience": string, "stsRegions": []string}`, `SubjectConstraints {"accountIds": []string, "roleNames": []string}`, `AgentIDTemplate "aws:{account}:{instanceId}"`.
  - gcp: `ProviderConfig {"audience": string}`, `SubjectConstraints {"projectIds": []string, "serviceAccounts": []string}`, template `"gcp:{project}:{instanceId}"`.
  - azure: `ProviderConfig {"tenantId": string, "audience": string}`, `SubjectConstraints {"subscriptionIds": []string, "resourceGroups": []string, "managedIdentityObjectIds": []string}`, template `"azure:{subscription}:{resourceGroup}:{vmName}"`.
  - Go types in `internal/store`: `AWSPolicyConfig`, `AWSSubjectConstraints`, `GCPPolicyConfig`, `GCPSubjectConstraints`, `AzurePolicyConfig`, `AzureSubjectConstraints` with those JSON tags, and `func DecodeCloudPolicy[C, S any](p AgentEnrollmentPolicy) (C, S, error)` (or three explicit decoders; implementer's choice, exported for the controller).
  - API request gains (all `omitempty`): `audience`, `stsRegions`, `accountIds`, `roleNames`, `projectIds`, `tenantId`, `subscriptionIds`, `resourceGroups`, `managedIdentityObjectIds`. The k8s fields and JSON for a kubernetes policy are unchanged.
- [ ] **Step 1: Failing tests.** Store: each provider's minimal valid policy upserts and reads back; missing required field → error naming it, exactly: `aws enrollment policy requires audience, accountIds and roleNames`, `gcp enrollment policy requires audience, projectIds and serviceAccounts`, `azure enrollment policy requires tenantId, audience, subscriptionIds and managedIdentityObjectIds`; unknown provider → `unsupported enrollment provider "x"`; a wrong template for a provider → `unsupported agent ID template`; an `stsRegions` entry not matching the region regex (ruling 3) → `invalid stsRegions entry "x"`; migration 026 test (a cloud row inserts after 026; down migration). Controller: `POST /api/v1/agent-enrollment-policies` with `provider: gcp` → 201 and `GET` returns the gcp fields; validation errors are 400 `invalid enrollment policy: <msg>`. CLI: `agent enrollment-policy create p --provider aws --audience x --account-id 123456789012 --role-name ci-agent [--sts-region eu-west-1]`, `--provider gcp --audience x --project-id my-ci --service-account ci@my-ci.iam.gserviceaccount.com`, `--provider azure --tenant-id <id> --audience x --subscription-id <id> [--resource-group ci] --managed-identity-object-id <id>`; `--provider` defaults to `kubernetes`, and `--cluster/--namespace/--service-account` are required only for kubernetes (enforced in RunE, not `MarkFlagRequired`); `--agent-id-template` defaults per provider. Metrics: `agentAuthProvider("aws"|"gcp"|"azure")` returns itself.
- [ ] **Step 2:** Run `go test ./internal/store/ ./internal/controller/ ./internal/cli/ ./internal/metrics/ -run 'EnrollmentPolicy|Migration|AgentAuth' -count=1`; fail.
- [ ] **Step 3:** Implement; keep the kubernetes validation messages unchanged.
- [ ] **Step 4:** Re-run; `go vet ./...`.
- [ ] **Step 5:** Docs: `reference/cli.md` enrollment-policy section (per-provider flags). Commit `feat(agent): enrollment policies for aws, gcp and azure`.

### Task 5: Cloud exchange in `POST /api/v1/agents/enroll` + GCP verifier

**Files:**
- Create: `internal/controller/agent_enrollment_cloud.go` (interface, generic handler, GCP verifier), `internal/controller/jwks_test_helper_test.go` (test JWKS/issuer server and token signer with go-jose)
- Modify: `internal/api/agent_auth.go` (`AgentEnrollRequest.AWS *AWSSignedRequest json:"aws,omitempty"`; `type AWSSignedRequest struct { Method string json:"method"; URL string json:"url"; Headers map[string][]string json:"headers"; Body string json:"body" /* base64 */ }`), `internal/controller/api_agent_enrollment.go` (`handleAgentEnroll` dispatch), `internal/controller/server.go` (`Config.CloudEnrollmentVerifiers`, `Server.cloudEnrollmentVerifiers`; `NewServer` fills missing providers with the real verifiers)
- Test: `internal/controller/agent_enrollment_cloud_test.go`

**Interfaces:**
- Produces:
  ```go
  type CloudEnrollmentProof struct { BearerToken string; AWS *api.AWSSignedRequest }
  type CloudEnrollmentIdentity struct { AgentID string } // also used as ExternalSubject
  type CloudEnrollmentVerifier interface {
      Verify(ctx context.Context, proof CloudEnrollmentProof, policy store.AgentEnrollmentPolicy) (CloudEnrollmentIdentity, error)
  }
  var ErrCloudEnrollmentRejected = errors.New("enrollment policy rejected")
  var ErrCloudEnrollmentUnavailable = errors.New("identity unavailable")
  func NewGCPEnrollmentVerifier(issuer, jwksURL string, client *http.Client) CloudEnrollmentVerifier
  // real: issuer "https://accounts.google.com", jwksURL "https://www.googleapis.com/oauth2/v3/certs"
  ```
- Handler `func (s *Server) handleCloudAgentEnroll(w, r, req api.AgentEnrollRequest)` follows `handleKubernetesAgentEnroll` step for step (rate limit with provider name, policy lookup, `policy.Provider == req.Provider` else 403, verifier lookup else 503, `authorizedKubernetesRequest` for labels, `IssueExternalAgentAccess{AgentID: id.AgentID, EnrollmentMethod: req.Provider, ExternalSubject: id.AgentID, ...}`, `recordAgentAuth(req.Provider, ...)`, audit, 200 `AgentTokenResponse` without refresh). `handleAgentEnroll`: `kubernetes` → existing; `aws|gcp|azure` → cloud; any other non-empty provider → `recordAgentAuth("other","failure","policy")` + 403 `enrollment policy rejected`; empty → one-time-token path as today.
- GCP checks: go-oidc `oidc.NewVerifier(issuer, oidc.NewRemoteKeySet(ctx, jwksURL), &oidc.Config{ClientID: audience})`; claims `email`, `email_verified` must be true, `google.compute_engine.project_id`, `.instance_id`; project in `projectIds`, email in `serviceAccounts`; agent ID `gcp:<project_id>:<instance_id>`. A JWKS fetch failure → `ErrCloudEnrollmentUnavailable`.
- [ ] **Step 1: Failing tests** with the test issuer: valid token → 200, agent ID `gcp:my-ci:1234567890123456789`, labels = required + allowed requested; wrong `aud` → 403; other issuer → 403; expired → 403; unknown `kid` → 403; project not listed → 403; service account not listed → 403; `email_verified` false → 403; JWKS server down → 503 `gcp identity unavailable`; requested label not allowed → 403; disabled identity on re-proof → 403 `agent identity disabled`; second exchange after the policy dropped an allowed label returns the reduced label set (Review Focus 4); policy for `aws` used with `provider: gcp` → 403; unknown provider `"ibm"` → 403; kubernetes request JSON unchanged (marshal golden: `{"provider":"kubernetes","policy":"p"}` has no `aws` key); metrics recorded under `gcp`.
- [ ] **Step 2:** Run `go test ./internal/controller/ -run 'CloudEnroll|GCP|AgentEnroll' -count=1`; fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Re-run, plus the existing k8s enrollment tests unchanged.
- [ ] **Step 5:** Commit `feat(agent): cloud identity exchange with a GCP verifier`.

### Task 6: AWS verifier

**Files:**
- Create: `internal/controller/agent_enrollment_aws.go`, `internal/controller/agent_enrollment_aws_test.go`

**Interfaces:**
- Consumes: Task 5 interface and `api.AWSSignedRequest`; Task 4 `AWSPolicyConfig`/`AWSSubjectConstraints`.
- Produces: `func NewAWSEnrollmentVerifier(client *http.Client, resolveHost func(host string) string) CloudEnrollmentVerifier` (`resolveHost` nil in production; tests map `sts.amazonaws.com` to an `httptest.NewTLSServer` URL).
- Checks, in order (any failure → `ErrCloudEnrollmentRejected` unless noted): proof present; method `POST`; URL scheme `https`, path `/` or empty, no query, host allowed per ruling 3; body (base64-decoded) equals `Action=GetCallerIdentity&Version=2011-06-15`; header `X-Unified-CD-Audience` equals policy `audience`; the `Authorization` header's `SignedHeaders` list contains `x-unified-cd-audience`; forward the request (same method, headers, body) to the host with a 5s timeout: network error or 5xx → `ErrCloudEnrollmentUnavailable`, other non-200 → rejected; parse `GetCallerIdentityResult/Arn`; ARN must be `arn:aws:sts::<12 digits>:assumed-role/<role>/<session>` (also `arn:aws-us-gov` / `arn:aws-cn` partitions); session matches `^i-[0-9a-f]{8,17}$`; account in `accountIds`; role in `roleNames` (exact, case-sensitive). Agent ID `aws:<account>:<session>`.
- [ ] **Step 1: Failing tests** (fake STS returns canned XML): valid → identity `aws:123456789012:i-0abc1234def567890`; disallowed host `evil.example.com` → rejected and the fake records no request (SSRF); region host `sts.eu-west-1.amazonaws.com` allowed when `stsRegions` empty or contains it, rejected when `stsRegions: [us-east-1]`; wrong body; audience header missing; audience header present but not in `SignedHeaders`; audience mismatch; STS 403 → rejected; STS 500 → unavailable; ARN for an IAM user (`arn:aws:iam::...:user/x`) → rejected; session `botocore-session-1` → rejected; account/role not listed → rejected.
- [ ] **Step 2–4:** Run `go test ./internal/controller/ -run AWS -count=1`, implement, re-run.
- [ ] **Step 5:** Commit `feat(agent): AWS STS identity verifier`.

### Task 7: Azure verifier

**Files:**
- Create: `internal/controller/agent_enrollment_azure.go`, `internal/controller/agent_enrollment_azure_test.go`

**Interfaces:**
- Produces: `func NewAzureEnrollmentVerifier(authority string, client *http.Client) CloudEnrollmentVerifier` (production `authority = "https://login.microsoftonline.com"`; JWKS `<authority>/<tenantId>/discovery/v2.0/keys`).
- Checks: issuer is `https://sts.windows.net/<tenantId>/` or `<authority>/<tenantId>/v2.0` (Review Focus 1); `aud` equals policy `audience`; `tid` equals `tenantId`; `oid` in `managedIdentityObjectIds`; `xms_mirid` parses per ruling 4 (case-insensitive segment names); subscription in `subscriptionIds` (case-insensitive); if `resourceGroups` non-empty the RG is in it (case-insensitive). Agent ID lower-cased (Review Focus 2).
- [ ] **Step 1: Failing tests:** v1 and v2 issuers both pass; other tenant's issuer → rejected; VMSS `xms_mirid` (`.../virtualMachineScaleSets/x`) → rejected; user-assigned (`.../Microsoft.ManagedIdentity/userAssignedIdentities/x`) → rejected; `RESOURCEGROUPS`/upper-case RG matches `resourceGroups: [ci]` and yields `azure:<sub>:ci:vm-01`; oid/subscription/RG not listed → rejected; JWKS down → unavailable.
- [ ] **Step 2–4:** Run `go test ./internal/controller/ -run Azure -count=1`, implement, re-run.
- [ ] **Step 5:** Commit `feat(agent): Azure managed identity verifier`.

### Task 8: Host-agent cloud credential source

**Files:**
- Create: `internal/agent/cloud_credentials.go`, `internal/agent/cloud_proof.go` (one function per provider), `internal/agent/cloud_credentials_test.go`

**Interfaces:**
- Produces:
  ```go
  type CloudCredentialSourceConfig struct {
      Server, Provider, Policy, Audience string
      Labels, Capabilities []string
      HTTPClient *http.Client
      MetadataBaseURL string // "" = provider default; tests point it at httptest
      STSHostOverride string // tests only: replaces the STS host the agent signs for
      Now func() time.Time; Jitter func() time.Duration
  }
  func NewCloudCredentialSource(cfg CloudCredentialSourceConfig) (*CloudCredentialSource, error) // error on unknown provider or empty policy/audience
  func (s *CloudCredentialSource) Token(ctx context.Context) (string, error)
  func (s *CloudCredentialSource) Invalidate()
  func (s *CloudCredentialSource) AgentID() string
  func (s *CloudCredentialSource) Labels() []string
  ```
  Same caching, 15m lead + jitter, 3-attempt retry and generic errors as `KubernetesCredentialSource` (copy the logic; do not refactor the k8s package).
- Proofs (fetched fresh for every exchange):
  - gcp: `GET <base or http://metadata.google.internal>/computeMetadata/v1/instance/service-accounts/default/identity?audience=<aud>&format=full` with `Metadata-Flavor: Google` → Bearer.
  - azure: `GET <base or http://169.254.169.254>/metadata/identity/oauth2/token?api-version=2018-02-01&resource=<aud>` with `Metadata: true` → `access_token` → Bearer.
  - aws: IMDSv2 (`PUT /latest/api/token` with `X-aws-ec2-metadata-token-ttl-seconds: 60`), region from `/latest/meta-data/placement/region`, role from `/latest/meta-data/iam/security-credentials/`, credentials from `.../<role>`; build `POST https://sts.<region>.amazonaws.com/` with body `Action=GetCallerIdentity&Version=2011-06-15`, headers `Content-Type: application/x-www-form-urlencoded; charset=utf-8`, `X-Unified-CD-Audience: <aud>`, `X-Amz-Security-Token`; SigV4 sign for service `sts` and the region with `x-unified-cd-audience` in SignedHeaders. Use `minio-go/pkg/signer.SignV4WithServiceType` only if a test proves it signs that header; otherwise write a minimal SigV4 signer in `internal/agent/sigv4.go` tested against the AWS SigV4 test-suite vector `get-vanilla` (or an equivalent published vector).
- [ ] **Step 1: Failing tests:** for each provider, an httptest metadata server + httptest controller that asserts the enroll body (`provider`, `policy`, `labels`, `aws` object only for aws) and returns a token response; `AgentID()`/`Labels()` reflect the response; a cached token is reused until the lead time; `Invalidate()` forces a new proof; 401 from the controller is retried, 403 is not; the AWS proof's `Authorization` header lists `x-unified-cd-audience` in SignedHeaders and the controller-side `NewAWSEnrollmentVerifier` (Task 6) accepts it end to end against the fake STS (cross-task test placed in `internal/controller` if import direction requires).
- [ ] **Step 2–4:** Run `go test ./internal/agent/ -run Cloud -count=1`, implement, re-run.
- [ ] **Step 5:** Commit `feat(agent): cloud identity credential source for the host agent`.

### Task 9: Agent flags, config and startup wiring

**Files:**
- Modify: `internal/config/agent.go` (`EnrollmentProvider` yaml `enrollmentProvider` env `UNIFIED_AGENT_ENROLLMENT_PROVIDER`; `EnrollmentPolicy` yaml `enrollmentPolicy` env `UNIFIED_AGENT_ENROLLMENT_POLICY`; `EnrollmentAudience` yaml `enrollmentAudience` env `UNIFIED_AGENT_ENROLLMENT_AUDIENCE`), `cmd/unified-cd-agent/main.go` (flags `--enrollment-provider`, `--enrollment-policy`, `--enrollment-audience`; help text ends with `(env: ...)`)
- Test: `internal/config/config_test.go`, `cmd/unified-cd-agent/main_test.go`, new `internal/controller/cloud_enroll_e2e_test.go` (ruling 7)

**Interfaces:**
- Consumes: `NewCloudCredentialSource` (Task 8), `NewGCPEnrollmentVerifier` (Task 5).
- Behaviour: provider set → `NewCloudCredentialSource` with `Labels` from `--labels` (the label request) and capabilities as the agent already detects them; one `Token` call at startup (30s, like `EnsureIdentity`), then `agentID := source.AgentID()`, `labels := source.Labels()`, `agent.NewWithLabels(agentID, labels, cli)`. Provider unset → today's path untouched.
- Startup errors (exit non-zero, exact strings): provider not in `aws|gcp|azure` → `--enrollment-provider must be aws, gcp or azure`; provider set without policy or audience → `--enrollment-provider requires --enrollment-policy and --enrollment-audience`; provider together with `--enrollment-token`, `--enrollment-token-file`, `--credential-file` or `--id` → `--enrollment-provider cannot be combined with --enrollment-token, --enrollment-token-file, --credential-file or --id` (Review Focus 3).
- [ ] **Step 1: Failing tests:** config env/file/flag precedence for the three keys, `KnownFields` accepts them; each startup error; in-process e2e: `NewServer` with a GCP verifier on the test JWKS issuer, a gcp policy created through the API, a `CloudCredentialSource` against a fake metadata server, then `register` and one claim poll succeed with the authorized labels.
- [ ] **Step 2–4:** Run `go test ./internal/config/ ./cmd/unified-cd-agent/ ./internal/controller/ -run 'Enrollment|Cloud' -count=1`, implement, re-run.
- [ ] **Step 5:** Commit `feat(agent): --enrollment-provider for cloud VM enrollment`.

### Task 10: Docs, examples and TODO for cloud enrollment

**Files:**
- Modify: `docs/operator-manual/agents.md` (section "Cloud VM enrollment (AWS, GCP, Azure)": how it works, per-provider IdP-side steps — AWS instance profile with a trust policy allowing only `ec2.amazonaws.com` and the session-name risk; GCP VM service account; Azure system-assigned identity and an app registration whose Application ID URI is the audience; policy commands; agent flags; the "verified only against simulated responses" note; limitations: VMSS and user-assigned identities, tags), `docs/reference/configuration.md` (agent flags/env/YAML table rows and example), `docs/reference/cli.md` (cross-link), `docs/troubleshooting/agents-and-enrollment.md` (cloud 403/503 strings, startup errors), `docs/operator-manual/authentication.md` (pointer), `examples/config/agent.yaml` (commented cloud block), `TODO.md` (new items: prune external identities unseen for a long time, k8s and cloud; Azure VMSS and user-assigned identities; VM tags as a policy condition — each with Impact/Evidence/Done-when in the file's format), `README.md` feature summary if it lists enrollment methods.
- [ ] **Step 1:** Write the docs from the behaviour implemented in Tasks 4–9 (re-read their final code; quote exact strings).
- [ ] **Step 2:** Grep `docs/ examples/ README.md charts/` for statements now wrong ("only Kubernetes", "provider kubernetes", "agent.manage" advice for provisioners) and fix them.
- [ ] **Step 3:** mkdocs build; `go test ./internal/dsl/ -run 'Templates|Examples' -count=1`.
- [ ] **Step 4:** Commit `docs: cloud VM agent enrollment`.
