# PR D — Capabilities auto-detected only — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`).

**Goal:** Make an agent's capabilities come from its own runtime auto-detection (`native` always; `container` if a container runtime is present), not from admin-set enrollment. Remove the `--capability` admin surface; the controller trusts the (authenticated, enrolled) agent's self-reported, vocabulary-validated capabilities.

**Architecture:** After PR C, `handleAgentRegister` seeds capabilities from `principal.AuthorizedCapabilities` (enrollment-set) and ignores the agent's self-reported `req.Capabilities`. PR D flips this: use `req.Capabilities` (validated via `dsl.ValidCapability`), and retire the enrollment-set capability path (CLI flags + API request fields). The agent already auto-detects (`internal/agent/agent.go:135 agentCapabilities`, unchanged). DB `authorized_capabilities` columns are left in place but unused (a migration to drop them is an optional follow-up).

**Tech Stack:** Go 1.26.2, chi, cobra, Postgres.

## Global Constraints

- Module `github.com/eirueimi/unified-cd`. No cgo/gcc → tests without `-race`.
- Capabilities are a **technical capability advertisement** (what the agent can run), not an authorization boundary — trusting the authenticated agent's validated self-report is the intended model. LABELS remain admin-authorized (unchanged — do NOT touch label handling).
- **No DB migration** in this PR: leave the `authorized_capabilities` columns (policies/tokens/identities) in place, unused. (Optional follow-up migration to drop them.) Do not change label columns.
- Keep `dsl.ValidCapability` validation of advertised capabilities.
- Commit trailer `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`. Repo root; prefix Bash with `cd … && …`.

---

### Task 1: Register uses the agent's auto-detected capabilities

**Files:**
- Modify: `internal/controller/api_agent.go` (`handleAgentRegister`)
- Test: `internal/controller/api_agent_capabilities_test.go` (or `api_agent_test.go`)

**Interfaces:**
- Consumes: `req.Capabilities` (the agent's auto-detected set) + `dsl.ValidCapability`.
- Produces: the persisted agent inventory row's `capabilities` reflect what the agent reported, not the enrollment token.

- [ ] **Step 1: Write the failing test (RED)**

Add a controller test: enroll an agent (mint a `uca_` token whose identity has EMPTY authorized capabilities), then register with `req.Capabilities = ["native","container"]`; assert the persisted agent (via `GetAgent`/list or `ClaimNextRun` matching a `container`-required job) has `container`. Pre-change this fails because register uses the (empty) `principal.AuthorizedCapabilities`. Mirror the enrolled-register test setup the other `api_agent` tests now use (minted `uca_`). A second case: register with `["native"]` only → no `container`.

- [ ] **Step 2: Run it — expect FAIL (RED)**

Run: `go test ./internal/controller/ -run 'Register.*[Cc]apabilit' -count=1 -v`
Expected: FAIL (persisted caps are the empty authorized set, not the reported ones). (Postgres; rerun once on `[setup failed]`.)

- [ ] **Step 3: Change the register handler (GREEN)**

In `internal/controller/api_agent.go` `handleAgentRegister`, replace:
```go
	capabilities := append([]string(nil), principal.AuthorizedCapabilities...)
```
with:
```go
	// Capabilities are the agent's own runtime auto-detection (native; container
	// if a container runtime is present) — a technical advertisement of what it
	// can run, not an authorization boundary. Trust the authenticated agent's
	// self-report, validated against the known vocabulary below. (Labels, by
	// contrast, remain controller-authorized above.)
	capabilities := append([]string(nil), req.Capabilities...)
```
Keep the existing `for _, c := range capabilities { if !dsl.ValidCapability(c) { 400 } }` validation and the `UpsertAgent(..., capabilities, ...)` call unchanged.

- [ ] **Step 4: Run it — expect PASS (GREEN)**

Run: `go test ./internal/controller/ -run 'Register.*[Cc]apabilit' -count=1 -v`
Expected: PASS. Then the broader agent suite:
Run: `go test ./internal/controller/ -run 'TestAgentAPI' -count=1`
Expected: PASS (adjust any test that asserted enrollment-set caps on register — repoint it to the reported caps).

- [ ] **Step 5: Commit** (`feat(agent): capabilities come from the agent's runtime auto-detection, not enrollment`).

---

### Task 2: Remove the admin `--capability` surface + docs

**Files:**
- Modify: `internal/cli/agent_enrollment.go` (`enrollment create`, `enrollment-policy create/update`, identity display)
- Modify: `internal/api/agent_auth.go` (drop `Capabilities` from `CreateAgentEnrollmentRequest`; drop `Capabilities` from `AgentEnrollmentPolicyRequest` — leave the response/meta `AuthorizedCapabilities` field but it is now always empty)
- Modify: `internal/controller/api_agent_enrollment.go` (stop threading the removed request `Capabilities` into the token/policy — pass empty)
- Modify: `docs/cli.md`, `docs/agents.md`, `docs/configuration.md` (capabilities section)
- Tests: `internal/cli/agent_enrollment_test.go`, controller enrollment tests

**Interfaces:**
- Consumes: nothing new.
- Produces: `enrollment create` / `enrollment-policy` no longer accept `--capability`; the enrollment token/policy carry no capabilities; identity display no longer shows authorized capabilities.

- [ ] **Step 1: Remove the CLI flags**

In `internal/cli/agent_enrollment.go`: delete the `--capability` flag registrations (the VM `enrollment create` one ~line 179, and the k8s `enrollment-policy` one ~line 69) and the `capabilities` locals; stop passing `Capabilities:` in the `CreateAgentEnrollmentRequest` (~143) and `AgentEnrollmentPolicyRequest` (~44) literals. In the identity-display (`agent identity get`, ~303-305), drop the `Capabilities: %s` line (the identity no longer carries authorized capabilities; the agent's live capabilities are visible via `agent get`/`list` from the inventory row).

- [ ] **Step 2: Remove the API request fields**

In `internal/api/agent_auth.go`: remove the `Capabilities []string` field from `CreateAgentEnrollmentRequest` and from `AgentEnrollmentPolicyRequest`. KEEP `AgentInfo.Capabilities` (the inventory row, populated from the agent's report — that's the live capabilities) and the `AgentTokenResponse`/register `req.Capabilities` field (the agent still SENDS its detected caps). In `internal/controller/api_agent_enrollment.go`, stop reading the removed request field (the token/policy get empty `AuthorizedCapabilities`).

- [ ] **Step 3: Build + fix fallout**

Run: `go build ./... && go vet ./...`
Fix every compile error from the removed fields (store structs may still have `AuthorizedCapabilities` — that's fine, they now receive empty; only the request-side reads are removed). Do NOT remove the store `AuthorizedCapabilities` struct fields or DB columns (left unused per Global Constraints).

- [ ] **Step 4: Tests**

Update/remove tests that set `--capability` or assert enrollment-carried capabilities: `agent_enrollment_test.go` (the create/policy tests), and any controller test asserting a token/policy's `AuthorizedCapabilities`. Add/keep: `agent enrollment create --help` shows no `--capability`; creating a token then enrolling yields an identity with empty authorized capabilities (capabilities now come from register).
Run: `go test ./internal/cli/ ./internal/controller/ -count=1`
Expected: PASS.

- [ ] **Step 5: Docs**

`docs/cli.md`: remove `--capability` from `agent enrollment create` / `enrollment-policy`; state capabilities are auto-detected by the agent. `docs/agents.md` capabilities section: explain `native` always + `container` when a container runtime is present, auto-detected and self-reported (no admin setting). `docs/configuration.md`: drop any `--capability`/authorized-capabilities enrollment guidance. Keep the labels docs unchanged.

- [ ] **Step 6: Final check + commit**

Run: `grep -rn "\-\-capability\|authorized.capabilit" docs/ internal/cli/ internal/api/agent_auth.go --include=*.go --include=*.md | grep -v _test | grep -v superpowers` → only legitimate residue (e.g. store/meta `AuthorizedCapabilities` field kept unused, or the response meta). Report anything ambiguous. Then:
Run: `go build ./... && go test ./... -short -count=1`
Expected: green.
Commit (`feat(cli): remove --capability from enrollment; capabilities are auto-detected`).

---

## Self-Review

**Spec coverage (spec §D):** register uses agent-reported caps → Task 1. Remove `--capability` from enrollment create (+policy) → Task 2 Step 1. Drop capabilities from the enrollment request/identity output → Task 2 Steps 1-2. Docs → Task 2 Step 5. Leave DB columns unused (no migration) → Global Constraints. ✓

**Placeholder scan:** exact file:line + the exact handler substring change given; test shapes described concretely.

**Type consistency:** `req.Capabilities` (already exists on the register request, sent by the agent) is what register now reads. Store `AuthorizedCapabilities` fields/columns stay (unused). API `CreateAgentEnrollmentRequest.Capabilities` / `AgentEnrollmentPolicyRequest.Capabilities` are removed; `AgentInfo.Capabilities` (inventory) stays.

**Interaction with PR C:** PR C left the register handler using `principal.AuthorizedCapabilities` and deferred the empty-caps NULL question here. With Task 1, an enrolled agent always reports ≥`native`, so its inventory caps are non-empty — the NULL-caps case no longer arises for a registered agent (the `capabilities IS NULL` claim semantics remain for the pre-existing unregistered-claim edge case, untouched).

**Ordering:** Task 1 (handler) then Task 2 (surface) — Task 1 makes caps functional from the report; Task 2 removes the now-dead admin surface. Task 2 alone would leave register still reading the (soon-empty) authorized set.
