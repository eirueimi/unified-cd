# PR C — Remove ALL legacy agent auth (VM shared-token + k8s static-token) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`).

**Goal:** Remove both deprecated agent-auth paths entirely — the VM legacy shared token (`UNIFIED_AGENT_LEGACY_TOKEN` / `--token` / `AuthMethod=="legacy"`) and the Kubernetes static token (`--secret` / `UNIFIED_K8S_SECRET`) — so enrollment (VM) and workload/projected-ServiceAccount enrollment (k8s) are the ONLY agent auth models.

**Architecture:** A read-only mapping audit produced the exhaustive removal map at `.superpowers/sdd/legacy-auth-removal-map.md` — **every task MUST read the relevant section of that map for the precise file:line list.** Key cross-cutting fact: the k8s static token authenticates through the SAME controller legacy branch (`agent_auth.go`), so all controller-side removal lives under the VM path; the k8s path is agent-binary + config + docs only. **No DB schema change.**

**Tech Stack:** Go 1.26.2, chi router, cobra, prometheus, Postgres (integration/e2e tests).

## Global Constraints

- Module path `github.com/eirueimi/unified-cd`. No cgo/gcc → tests without `-race`.
- **Read `.superpowers/sdd/legacy-auth-removal-map.md`** — it is the detailed spec; the plan orchestrates it. Sections: A.1-A.8 (VM), B.1-B.3 (k8s).
- **KEEP (do NOT remove):** `UNIFIED_TOKEN` (human admin PAT), workload/projected-SA enrollment, `UNIFIED_K8S_CONFIG`/`--config`, `NewClient`/`staticTokenSource` (generic), and the out-of-scope "legacy" concepts (null-caps, data records, job re-key, bodyless heartbeat, webhook shared token, admin static token).
- **phase8's `mustSeedBootstrapPAT` / `Token:"static-token"` is the HUMAN PAT path — KEEP.**
- **Repo-wide scan gate (final task):** after the whole PR, `grep -rniE "legacy.?agent|legacysharedtoken|UNIFIED_AGENT_LEGACY_TOKEN|UNIFIED_AGENT_TOKEN|UNIFIED_K8S_SECRET|AgentLegacyAuth|AuthMethod.*legacy" . --include=*.go --include=*.md --include=*.yaml --include=*.yml --include=*.toml | grep -v "docs/superpowers/" | grep -v vendor/ | grep -v "\.superpowers/"` must be empty EXCEPT deliberately-kept out-of-scope null-caps/data-record "legacy" mentions (which don't match these patterns anyway).
- Commit trailer `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`. Repo root; prefix Bash with `cd … && …`.

---

### Task 1: Migrate tests off the legacy bearer (while legacy still works)

Switch every test that uses `LegacyAgentToken`/static-token merely as an **agent bearer to exercise other behavior** over to a minted `uca_` access token (the enrolled path, which already works with the legacy code still present). This makes the later code-removal tasks keep the suite green. Do NOT yet touch tests that specifically assert "legacy is accepted/rejected" (those go with the code removal in Task 2).

**Files:** the test files in map §A.7 that use `LegacyAgentToken` as a vehicle — `internal/controller/agent_guard_test.go`, `api_artifacts_test.go`, `api_secrets_test.go`, `api_jobs_test.go`, `metrics_integration_test.go`, and the ~40 `test/e2e/` sites (walking_skeleton, matrix_smoke, failing_step, artifact_roundtrip, phase2-9). Reuse the existing `issueAgentAccessToken` helper (`test/e2e/phase5_test.go:42`); if a controller-package equivalent is needed, add a small helper that enrolls an agent and returns a `uca_` access token.

- [ ] **Step 1: Read map §A.7 and the `issueAgentAccessToken` helper**; identify the exact "uses-legacy-as-bearer" sites vs the "asserts-legacy-semantics" tests (the latter are Task 2, leave them).
- [ ] **Step 2:** For each vehicle site, replace the `controller.Config{… LegacyAgentToken: X}` fixture + `Authorization: Bearer X` usage with an enrolled agent + minted `uca_` access token. Keep phase8's human-PAT path untouched.
- [ ] **Step 3:** Run the migrated suites: `go test ./internal/controller/ -count=1` and `go test ./test/e2e/... -count=1` (these need Postgres; rerun once on a `[setup failed]` flake). Expected: PASS with the legacy code still present (proves the tests no longer depend on legacy).
- [ ] **Step 4: Commit** (`test(auth): migrate agent-bearer tests off legacy token to minted uca_ access`).

---

### Task 2: Remove VM legacy controller code + config + cmd/controller + metrics

This is the core removal. Follow map **§A.1-A.5 and the legacy-assertion tests in §A.7**.

**Files:** `internal/controller/agent_auth.go`, `internal/controller/api_agent.go`, `internal/controller/api_secrets.go`, `internal/controller/server.go`, `internal/config/controller.go`, `cmd/controller/main.go`, `internal/metrics/metrics.go`, plus tests `agent_auth_test.go`, `metrics_test.go`, `config_test.go`, `cmd/controller/main_test.go`.

- [ ] **Step 1:** Read map §A.1-A.5. Apply the deletions/collapses exactly:
  - `agent_auth.go`: delete the post-`uca_` legacy block (§A.1: `LegacyAgentToken` compare, `AgentLegacyAuth()`, the `AuthMethod:"legacy"` principal) → a non-`uca_` bearer now 401s; collapse `requireAgentPathIdentity` (keep the `!ok` human early-return, delete only the `legacy` assignment branch, keep the enrolled `AgentID != {agentId}` 403); simplify `agentOrServerAuth` routing to `HasPrefix(token,"uca_")`. Leave `AuthMethod` always `"bearer"` (do not delete the field in this task — optional later).
  - `api_agent.go`: make the register `!= "legacy"` block unconditional (always enforce `agentId==principal.AgentID`, always use `principal.Authorized*`); delete the legacy `hostname:` label synthesis; collapse `handleAgentClaim` to always use authorized labels (drop `?labels=` read).
  - `api_secrets.go`: `if !ok || AuthMethod=="legacy"` → `if !ok`.
  - `server.go`/`config/controller.go`/`cmd/controller/main.go`: delete `LegacyAgentToken` field, `LegacySharedToken` config field/env/precedence, the `legacyAgentAuthWarning` call+func, and the `LegacyAgentToken:` literal.
  - `metrics.go`: delete the `agentLegacyAuth` counter field, construction, registration, and `AgentLegacyAuth()` method.
- [ ] **Step 2:** Update the legacy-assertion tests (map §A.7): flip `TestAgentAuth_LegacyTokenIsExplicit…`'s legacy-accept half to assert **401**; DELETE `TestLegacyAgentRequestIncrementsMigrationCounter`, `cmd/controller/main_test.go` `TestLegacyAgentAuthWarning`, the `metrics_test.go` `agentLegacyAuth` assertion, and the `config_test.go` legacy subtests (keep the `kubernetesClusters` validation coverage). Add a focused test: a shared-token bearer request → 401.
- [ ] **Step 3:** `go build ./... && go vet ./...` clean; `go test ./internal/controller/ ./internal/config/ ./internal/metrics/ ./cmd/controller/ -count=1` passes (Postgres for controller). This removal also stops the k8s static token from authenticating (intended; k8s binary/config cleanup is Task 4).
- [ ] **Step 4: Commit** (`feat(auth)!: remove VM legacy shared-token agent authentication`).

---

### Task 3: Remove the agent-side `--token` / `UNIFIED_AGENT_TOKEN`

Follow map **§A.4 (agent), §A.3 (agent config)**.

**Files:** `cmd/unified-cd-agent/main.go`, `internal/config/agent.go`, and any agent test referencing the token.

- [ ] **Step 1:** Delete the `--token` flag (`main.go:67`), and make the credential-manager path unconditional (delete the `if *token != "" { warn; NewClient } else { … }` split at `main.go:159-199`, drop the "using deprecated legacy shared agent token" warning). Delete `AgentConfig.Token` field, `UNIFIED_AGENT_TOKEN` env read, and the `file.Token` precedence block in `internal/config/agent.go` (§A.3).
- [ ] **Step 2:** Optional/low-risk per map §A.6: the `stepenv.go` `UNIFIED_AGENT_TOKEN` deny entry and the `runner_test.go` sample-env usage are cosmetic — update `runner_test.go` to a different sample var if it references `UNIFIED_AGENT_TOKEN`; the `stepenv.go` entry can be dropped (moot). Keep `NewClient`/`staticTokenSource`.
- [ ] **Step 3:** `go build ./... && go test ./cmd/unified-cd-agent/... ./internal/agent/... ./internal/config/ -count=1` passes.
- [ ] **Step 4: Commit** (`feat(agent)!: remove deprecated --token / UNIFIED_AGENT_TOKEN`).

---

### Task 4: Remove the k8s static-token path

Follow map **§B.1-B.2** (controller side is already gone via Task 2).

**Files:** `cmd/k8s-agent/main.go`, `internal/k8sagent/config.go`, `internal/k8sagent/config_test.go`.

- [ ] **Step 1:** `cmd/k8s-agent/main.go`: delete `--secret`/`UNIFIED_K8S_SECRET` flag; drop the `secretPath` arg to `LoadConfig`; in `bootstrapAgentClient` delete the `if cfg.Token != "" { NewClient }` static path (make `KubernetesCredentialSource` unconditional) and reword the comment. Keep `--config`/`UNIFIED_K8S_CONFIG`.
- [ ] **Step 2:** `internal/k8sagent/config.go`: delete `Config.Token` (`yaml:"token"`); drop `LoadConfig`'s `secretPath` param + the secret-overlay merge (keep the shared `loadYAML`); delete the `UNIFIED_K8S_AGENT_ID` override in `Validate` (drop the `yaml:"agentId"` input tag but keep the runtime-populated `AgentID` field); simplify `Validate` to `if c.EnrollmentPolicy == "" { error }` and delete the `Token!="" && AgentID==""` check.
- [ ] **Step 3:** `internal/k8sagent/config_test.go`: delete the secret-overlay tests (`TestLoadConfig_SecretOverridesToken`, `_MissingSecretFileIsSkipped`, `_EmptySecretPathIsSkipped`, and the `token:` half of `_LoadsFromFile`); move `sidecarS3SecretName` into the main-config load test; flip the `Validate` baseline tests that used `Token/AgentID` to `EnrollmentPolicy:"p"` (map §B.2 lists them). Keep the two canonical enrollment-config tests.
- [ ] **Step 4:** `go build ./... && go test ./internal/k8sagent/... ./cmd/k8s-agent/... -count=1` passes (non-k8s-tagged config tests; the `k8s`-tagged integration tests need kind and are exercised in CI).
- [ ] **Step 5: Commit** (`feat(k8s)!: remove legacy static-token (--secret) agent auth`).

---

### Task 5: Docs + final repo-wide scan

Follow map **§A.8 and §B.3** for the exact doc sites; do NOT touch the out-of-scope "legacy" concepts listed there.

**Files:** `docs/authentication.md`, `docs/authorization.md`, `docs/configuration.md`, `docs/agents.md`, `docs/cli.md`, `docs/getting-started.md`, `docs/high-availability.md`, `docs/audit.md`, and any compose-file comments.

- [ ] **Step 1:** Remove/rewrite each VM legacy-shared-token doc section (§A.8) and each k8s static-token doc section (§B.3) to "enrollment / workload enrollment is the only agent auth model". Keep the human `UNIFIED_TOKEN`, `--config`, timeout-env, and projected-SA prose. Rewrite `docs/authorization.md` so the per-run ownership guard applies to all bearer (enrolled) agents. Rewrite `docs/high-availability.md`'s `UNIFIED_K8S_AGENT_ID` scope-pod note to enrollment-derived AgentID.
- [ ] **Step 2: Final repo-wide scan (MUST be clean):**
  ```bash
  grep -rniE "legacy.?agent|legacysharedtoken|UNIFIED_AGENT_LEGACY_TOKEN|UNIFIED_AGENT_TOKEN|UNIFIED_K8S_SECRET|AgentLegacyAuth|--token|--secret" . \
    --include=*.go --include=*.md --include=*.yaml --include=*.yml --include=*.toml \
    | grep -v "docs/superpowers/" | grep -v vendor/ | grep -v "\.superpowers/"
  ```
  Expected: no output for the removed symbols. (Manually confirm any residual `--token`/`--secret` hits are unrelated — e.g. human `UNIFIED_TOKEN` docs use `--token` for the CLI admin token, which is a DIFFERENT flag on a different command; if such a legitimate hit exists, note it. The out-of-scope null-caps/data "legacy" mentions won't match these patterns.)
- [ ] **Step 3:** `go build ./... && go test ./... -short -count=1` green.
- [ ] **Step 4: Commit** (`docs: remove legacy agent-auth (VM shared-token + k8s static-token) guidance`).

---

## Self-Review

**Spec coverage:** VM controller (§A.1-A.5) → Task 2. Agent `--token` (§A.4/A.3) → Task 3. k8s static-token (§B.1-B.2) → Task 4. Docs (§A.8/B.3) → Task 5. Tests: vehicle-migration → Task 1, legacy-assertion flips → Task 2/4. ✓

**Placeholder scan:** The exhaustive file:line lists live in the committed-to-disk removal map (`.superpowers/sdd/legacy-auth-removal-map.md`); each task names the map section to follow plus the critical/risky specifics inline. This is intentional (the map is 130 lines of precise sites) — not a placeholder.

**Risk handling:** e2e blast radius → isolated in Task 1 (migrate first, while legacy still works, so removal stays green). Single-branch-two-modes → Task 2 removes the shared controller branch; Task 4 removes the k8s binary/config after. Collapse ordering in `requireAgentPathIdentity` → called out in Task 2 Step 1. No DB schema change.

**Ordering (strict):** 1 (migrate tests) → 2 (VM controller) → 3 (agent) → 4 (k8s) → 5 (docs+scan). Task 1 MUST precede 2 or the suite goes red.
