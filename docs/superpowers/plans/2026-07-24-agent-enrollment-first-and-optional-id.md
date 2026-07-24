# Agent enrollment-first credential resolution + optional `--id` — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make an explicitly-supplied enrollment token re-enroll a VM agent that already has a credential (so label updates apply), fall back to the existing credential when that token is expired/consumed (HTTP 401), and make `--id` optional by adopting the agent ID from the enrollment/credential.

**Architecture:** All credential-resolution logic lives in `internal/agent/credentials.go` (`CredentialManager`). `cmd/unified-cd-agent/main.go` resolves the agent's identity at startup via a new `EnsureIdentity` and threads it through instead of the `--id` flag. Design: `docs/superpowers/specs/2026-07-24-agent-enrollment-first-and-optional-id-design.md`.

**Tech Stack:** Go 1.26.2. No cgo → tests without `-race`.

## Global Constraints

- Module `github.com/eirueimi/unified-cd`. No cgo → run Go tests WITHOUT `-race`. Prefix Bash with `cd /c/Users/arimax/unified-cd-project/unified-cd && …`.
- **Fallback trigger is exactly HTTP 401** (`credentialRequestError.status == http.StatusUnauthorized`). 503/429/network (retryable) are retried by `exchangeWithRetry` and surfaced, never a fallback trigger. 403 (disabled) surfaces.
- Enroll is attempted **at most once per process** (guard with a `bootstrapDone` flag); after it resolves, only refresh runs.
- `--id` set → keep the identity **assertion** (mismatch = error). `--id` omitted → **adopt** the resolved `AgentID`. Never silently switch a configured identity.
- **Backward compat:** with `--id` set, the credential-file default path and all behavior are byte-for-byte unchanged.
- Controller, k8s-agent, and token formats are **out of scope** — do not touch them.
- Commit trailer (exact): `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`

---

### Task 1: CredentialManager — enrollment-first resolution, 401 fallback, optional-ID adopt, EnsureIdentity

**Files:**
- Modify: `internal/agent/credentials.go`
- Test: `internal/agent/credentials_test.go`

**Interfaces:**
- Consumes: existing `exchangeWithRetry`, `loadRefreshCredential`, `useCredential`, `validateCredential`, `credentialRequestError{status,retryable}`, `readSecretFile`, `persistedCredential{AgentID,...}`.
- Produces:
  - `func (m *CredentialManager) EnsureIdentity(ctx context.Context) (string, error)` — resolves and returns the agent ID (from the persisted credential without a network call when one exists; otherwise by enrolling).
  - Behavior change in `Token`: when an enrollment token is explicitly configured and bootstrap has not run, attempt `/enroll` first even if a credential exists; on a 401 with an existing credential, WARN and fall back to `/refresh`.
  - `validateCredential`/`useCredential`/`Token` adopt `AgentID` when `m.agentID == ""`.

- [ ] **Step 1: Write failing tests (RED)**

Add to `internal/agent/credentials_test.go`. Follow the existing harness there (a mock `http.Server`/`httptest` handling `/api/v1/agents/enroll` and `/api/v1/agents/token/refresh`, constructing `NewCredentialManager(CredentialManagerConfig{...})`, and a fake time via `Now`). Cover:

```go
// (a) With a valid persisted credential AND an enrollment token configured,
// Token() consumes the token (hits /enroll, not /refresh) — the re-enroll path.
// Assert the server observed a POST to /api/v1/agents/enroll and the returned
// access token is the one the enroll response supplied.

// (b) Enroll rejected 401 + credential present → fall back to /refresh.
// The enroll handler returns 401; the refresh handler returns a valid token.
// Assert Token() returns the refresh access token with no error, and that
// /refresh was called after /enroll.

// (c) Enroll rejected 401 + NO credential → Token() returns an error.

// (d) Enroll fails 503 (retryable) → Token() surfaces an error and does NOT
// call /refresh (no silent fallback). Use a handler that always 503s; assert
// the refresh endpoint was never hit.

// (e) --id omitted (AgentID:"" in config): after enroll, m.AgentID adopted from
// the response; EnsureIdentity returns it. Assert EnsureIdentity(ctx) == the
// response's agentId.

// (f) --id omitted, valid persisted credential present: EnsureIdentity resolves
// the ID from the credential file WITHOUT any HTTP call (assert the server got
// zero requests during EnsureIdentity).

// (g) --id set to a value that mismatches the response/credential AgentID →
// error (assertion preserved).

// (h) Enroll attempted at most once: after a successful enroll, force an
// access-token refresh (advance fake time past the lead time) and assert the
// second exchange hits /refresh, not /enroll.
```

Name them `TestCredentialManager_EnrollFirst_*` / `_Fallback401` / `_No503Fallback` / `_AdoptsAgentID` / `_EnsureIdentityOfflineFromCredential` / `_AssertsConfiguredID` / `_EnrollOnce`.

- [ ] **Step 2: Run — expect FAIL (RED)**

Run: `go test ./internal/agent/ -run 'TestCredentialManager_(EnrollFirst|Fallback401|No503Fallback|AdoptsAgentID|EnsureIdentity|AssertsConfiguredID|EnrollOnce)' -count=1 -v`
Expected: FAIL/compile error (`EnsureIdentity` undefined; enroll-first behavior absent — with a credential the code currently refreshes, so (a) fails).

- [ ] **Step 3: Add a `bootstrapDone` field + an enrollment-token resolver**

In `internal/agent/credentials.go`, add a field to the `CredentialManager` struct (near `loaded`):
```go
	bootstrapDone bool // true once the first enroll/refresh resolution has run
```
Add a helper (returns the configured enrollment token, or "" when none is configured; a configured-but-unreadable file surfaces its error):
```go
// enrollmentTokenValue returns the explicitly-configured one-time enrollment
// token (inline value preferred over file), or "" when none is configured.
func (m *CredentialManager) enrollmentTokenValue() (string, error) {
	if strings.TrimSpace(m.enrollmentToken) != "" {
		return strings.TrimSpace(m.enrollmentToken), nil
	}
	if m.enrollmentTokenFile != "" {
		v, err := readSecretFile(m.enrollmentTokenFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(v), nil
	}
	return "", nil
}
```

- [ ] **Step 4: Restructure the exchange decision in `Token` (enroll-first + 401 fallback)**

In `Token`, replace the current decision block (the `var response …; if m.refresh.RefreshToken != "" { refresh } else { enroll }` through the `if response.AgentID != m.agentID || …` invalid check) with:

```go
	var response api.AgentTokenResponse
	var err error
	enrollTok, tokErr := m.enrollmentTokenValue()
	switch {
	case !m.bootstrapDone && enrollTok != "":
		// An explicit enrollment token means "(re-)enroll" — prefer it even when
		// a credential already exists, so authorized-label changes take effect.
		response, err = m.exchangeWithRetry(ctx, "/api/v1/agents/enroll", enrollTok)
		if err != nil {
			var reqErr *credentialRequestError
			if errors.As(err, &reqErr) && reqErr.status == http.StatusUnauthorized && m.refresh.RefreshToken != "" {
				// The token is definitively rejected (expired/already consumed),
				// but we hold a working credential — keep running on it rather
				// than bricking. Labels are not updated in this case.
				slog.Warn("enrollment token rejected (expired or already consumed); continuing with the existing credential", "agentId", m.agentID)
				response, err = m.exchangeWithRetry(ctx, "/api/v1/agents/token/refresh", m.refresh.RefreshToken)
			}
		}
	case m.refresh.RefreshToken != "":
		response, err = m.exchangeWithRetry(ctx, "/api/v1/agents/token/refresh", m.refresh.RefreshToken)
	case tokErr != nil:
		return "", tokErr
	default:
		return "", fmt.Errorf("agent credentials are required")
	}
	if err != nil {
		return "", err
	}
	m.bootstrapDone = true
	// Adopt the agent ID from the response when --id was omitted; otherwise
	// assert the server agrees with the configured identity.
	if m.agentID == "" {
		m.agentID = response.AgentID
	} else if response.AgentID != m.agentID {
		return "", fmt.Errorf("credential response agent ID %q does not match configured agent ID %q", response.AgentID, m.agentID)
	}
	if response.AccessToken == "" || response.RefreshToken == "" || response.RefreshExpiresAt == nil {
		return "", fmt.Errorf("credential response is invalid")
	}
```

Keep everything after this (the `persistedCredential` build + `persist` + access-token assignment) unchanged. Ensure `errors`, `net/http`, `log/slog` are imported (add any missing).

- [ ] **Step 5: Adopt the ID on the credential-load path**

In `useCredential`, adopt the persisted ID when `--id` was omitted (so an offline restart resolves identity from the file):
```go
func (m *CredentialManager) useCredential(credential persistedCredential) error {
	if err := m.validateCredential(credential); err != nil {
		return err
	}
	if m.agentID == "" {
		m.agentID = credential.AgentID
	}
	m.refresh, m.loaded = credential, true
	return nil
}
```
And in `validateCredential`, only assert when an ID is configured:
```go
func (m *CredentialManager) validateCredential(credential persistedCredential) error {
	if m.agentID != "" && credential.AgentID != m.agentID {
		return fmt.Errorf("credential file agent ID %q does not match configured agent ID %q", credential.AgentID, m.agentID)
	}
	if !credential.RefreshExpiresAt.After(m.now()) {
		return fmt.Errorf("agent refresh credential has expired")
	}
	return nil
}
```

- [ ] **Step 6: Add `EnsureIdentity`**

Resolve identity from the local credential without a network call when possible; only enroll (network) when there is no credential and no configured ID:
```go
// EnsureIdentity resolves the agent's canonical ID before the run loop starts.
// With --id set it is returned as-is. With --id omitted it is adopted from the
// persisted credential (no network) when one exists, or by performing the
// first enrollment when there is none.
func (m *CredentialManager) EnsureIdentity(ctx context.Context) (string, error) {
	m.mu.Lock()
	if m.agentID != "" {
		id := m.agentID
		m.mu.Unlock()
		return id, nil
	}
	err := m.loadRefreshCredential()
	id := m.agentID
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil // adopted from the persisted credential, no network
	}
	// No credential and no configured ID → must enroll to learn the identity.
	if _, err := m.Token(ctx); err != nil {
		return "", err
	}
	m.mu.Lock()
	id = m.agentID
	m.mu.Unlock()
	return id, nil
}
```
(`loadRefreshCredential` is already `m.mu`-guarded by its callers; call it under the lock here as shown.)

- [ ] **Step 7: Run — expect PASS (GREEN)**

Run: `go test ./internal/agent/ -run 'TestCredentialManager' -count=1 -v`
Expected: PASS (new cases + existing CredentialManager tests). Fix any existing test that assumed the old refresh-preferred-over-enroll ordering or the old hard mismatch error string — repoint to the new messages above.
Then: `go test ./internal/agent/ -count=1`
Expected: PASS.

- [ ] **Step 8: Commit** (`feat(agent): re-enroll when a token is supplied, fall back to credential on 401, adopt agent ID`).

---

### Task 2: `--id` optional wiring — default credential path, startup identity resolution, docs

**Files:**
- Modify: `internal/config/agent.go` (`DefaultAgentCredentialFile`)
- Modify: `cmd/unified-cd-agent/main.go`
- Modify: `docs/agents.md`, `docs/cli.md`
- Test: `internal/config/*_test.go` (path helper)

**Interfaces:**
- Consumes: `CredentialManager.EnsureIdentity` (Task 1).
- Produces: `DefaultAgentCredentialFile("")` returns the ID-independent path; `main.go` resolves the agent ID from the credential manager and uses it for register/claim/reconcile.

- [ ] **Step 1: Default credential path for an omitted ID (RED)**

Add/adjust a test for `config.DefaultAgentCredentialFile`:
```go
// id set → ~/.unified-cd/<id>/credential.json (unchanged)
// id ""  → ~/.unified-cd/credential.json (no per-id segment)
func TestDefaultAgentCredentialFile_NoID(t *testing.T) {
	got, err := config.DefaultAgentCredentialFile("")
	require.NoError(t, err)
	home, _ := os.UserHomeDir()
	assert.Equal(t, filepath.Join(home, ".unified-cd", "credential.json"), got)
}
```
Run: `go test ./internal/config/ -run 'DefaultAgentCredentialFile' -count=1 -v` → FAIL (currently errors on empty id).

- [ ] **Step 2: Implement the path change (GREEN)**

In `internal/config/agent.go`, change `DefaultAgentCredentialFile` so an empty id yields the shared path instead of erroring:
```go
func DefaultAgentCredentialFile(id string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for default credential file: %w", err)
	}
	if strings.TrimSpace(id) == "" {
		return filepath.Join(home, ".unified-cd", "credential.json"), nil
	}
	return filepath.Join(home, ".unified-cd", id, "credential.json"), nil
}
```
Run: `go test ./internal/config/ -run 'DefaultAgentCredentialFile' -count=1` → PASS.

- [ ] **Step 3: main.go — make `--id` optional and resolve identity at startup**

In `cmd/unified-cd-agent/main.go`:
1. Change the `--id` default from the hostname to the config value (or empty). Where `defaultID` is currently derived from the hostname for the `id` flag, set the flag default to `eff.AgentID` (may be ""). Do NOT remove the separate hostname used for `req.Hostname` in registration (that stays `os.Hostname()` in `internal/agent/agent.go` — unrelated).
2. Leave the credential-path default block as-is except that `DefaultAgentCredentialFile(*id)` now accepts `*id == ""` (Step 2). When `*id == ""` and `--credential-file` unset, it resolves to `~/.unified-cd/credential.json`.
3. After `source := agent.NewCredentialManager(...)` and `cli := agent.NewClientWithTokenSource(...)`, resolve the effective agent ID before building the run loop:
```go
	resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 30*time.Second)
	agentID, err := source.EnsureIdentity(resolveCtx)
	resolveCancel()
	if err != nil {
		slog.Error("resolve agent identity", "error", err)
		os.Exit(1)
	}
	slog.Info("agent identity resolved", "agentId", agentID)
```
4. Replace the two later uses of `*id` — `cli.ReconcileRuns(fctx, *id)` and `agent.NewWithLabels(*id, labels, cli)` — with `agentID`.

- [ ] **Step 4: Build + smoke the flag change**

Run: `go build ./... && go vet ./...`
Expected: clean. Fix any reference to the removed hostname-default assumption.

- [ ] **Step 5: Docs**

- `docs/agents.md` (enrollment section): `--id` is optional — the agent adopts its ID from the enrollment token / persisted credential; supplying `--id` asserts it. Document that supplying a **new** enrollment token to an already-enrolled agent re-enrolls it (updating authorized labels), and that an **expired/consumed** token is ignored with a WARN (the agent keeps its existing credential). Note: running multiple agents on one host without `--id` or `--credential-file` collides on the single default credential path — set one of them.
- `docs/cli.md` (agent flags): update the `--id` description to "optional; defaults to the identity bound to the enrollment token / persisted credential".
- Update the pipe one-liner example to drop `--id` where the enrollment already fixes it:
  ```
  unified-cli agent enrollment create --agent-id agent-1 --label kind:linux --quiet \
    | unified-cd-agent --server https://ci.example.com --enrollment-token -
  ```

- [ ] **Step 6: Final check + commit**

Run: `go build ./... && go test ./... -short -count=1`
Expected: green (e2e phase tests skip on Windows; CI/Linux covers them).
Commit (`feat(agent): make --id optional; resolve identity from enrollment/credential + docs`).

---

## Self-Review

**Spec coverage:** enroll-first re-enroll (§A) → Task 1 Step 4; 401 fallback / 503-no-fallback (§A) → Task 1 Step 4 + tests (b)(c)(d); adopt/assert ID (§B) → Task 1 Steps 4–5; `EnsureIdentity` offline-from-credential (§B) → Task 1 Step 6 + test (f); default path + wiring + docs (§B) → Task 2. ✓

**Placeholder scan:** all code steps carry complete code; test cases enumerated with concrete assertions.

**Type consistency:** `EnsureIdentity(ctx) (string, error)` defined in Task 1 Step 6, consumed in Task 2 Step 3. `DefaultAgentCredentialFile(string) (string, error)` signature unchanged (Task 2 Step 2), only the empty-id branch added. `bootstrapDone`/`enrollmentTokenValue` introduced and used within Task 1.

**Ordering:** Task 1 (manager logic + EnsureIdentity) precedes Task 2 (which wires EnsureIdentity + the path helper into main.go). Task 2 Step 2's path change is independent and could run first, but is grouped with its consumer.

**Resilience note (verified against current flow):** eager `EnsureIdentity` resolves from the persisted credential with **no network call** on restart, so a controller-down-at-boot restart still starts. Only a true first enrollment (no credential) needs the controller — which is inherent (the agent has no identity yet).
