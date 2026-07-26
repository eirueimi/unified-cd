# Agent ID-Scoped Credential Path Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep `--id` optional while making a valid enrollment token authoritative for the agent ID and persisting default credentials only at `$HOME/.unified-cd/<agent-id>/credential.json`.

**Architecture:** Split default-path derivation from single-credential discovery in `internal/config`, then make `CredentialManager` resolve the credential destination only after a token response supplies the canonical ID. Explicit tokens run before implicit local credential discovery; token-less startup discovers exactly one ID-scoped credential or fails as ambiguous. The host-agent entrypoint delegates identity and path resolution to `CredentialManager` instead of creating a shared path before enrollment.

**Tech Stack:** Go 1.24, standard `flag`/`os`/`path/filepath`/`net/http`, `testify`, existing atomic credential persistence.

## Global Constraints

- Keep `--id` optional.
- A valid explicit enrollment token is authoritative unless `--id` explicitly asserts a different ID.
- Default credentials use only `$HOME/.unified-cd/<agent-id>/credential.json`.
- Never modify or delete credentials belonging to another agent ID.
- Ignore the legacy shared `$HOME/.unified-cd/credential.json` during implicit discovery.
- Preserve explicit `--credential-file` behavior.
- All repository text and commit messages must be English and contain no PII.
- Use TDD: observe each regression test fail before changing production code.
- Preserve unrelated changes; work only in the isolated `fix/agent-credential-path` worktree.

---

## File Structure

- `internal/config/agent.go`: derive an ID-scoped default path and discover one existing ID-scoped credential.
- `internal/config/agent_credential_test.go`: cross-platform unit tests for discovery using an isolated root.
- `internal/config/config_test.go`: update the public default-path contract test.
- `internal/agent/credentials.go`: token-first identity resolution, deferred path selection, and unambiguous 401 fallback.
- `internal/agent/credentials_test.go`: regression coverage for token authority, deferred persistence, discovery, and ambiguity.
- `cmd/unified-cd-agent/main.go`: remove eager shared-path creation and pass unresolved defaults to the credential manager.
- `docs/agents.md`, `docs/cli.md`, `docs/configuration.md`, `docs/troubleshooting.md`, `README.md`: update operator-facing behavior.
- `docs/migration-agent-id-scoped-credentials.md`: breaking-change migration guide.

---

### Task 1: ID-Scoped Path Derivation and Discovery

**Files:**
- Modify: `internal/config/agent.go`
- Modify: `internal/config/config_test.go`
- Create: `internal/config/agent_credential_test.go`

**Interfaces:**
- Produces: `DefaultAgentCredentialFile(id string) (string, error)`, which rejects an empty ID.
- Produces: `DiscoverDefaultAgentCredentialFile() (string, error)`, which searches the real default root.
- Produces: `discoverAgentCredentialFile(root string) (string, error)`, an internal deterministic discovery helper used by tests.

- [ ] **Step 1: Replace the shared-path assertions with a failing empty-ID test**

In `internal/config/config_test.go`, keep the non-empty assertion and replace
the empty-ID expectations with:

```go
func TestDefaultAgentCredentialFileRequiresID(t *testing.T) {
	for _, id := range []string{"", "   "} {
		_, err := config.DefaultAgentCredentialFile(id)
		require.EqualError(t, err, "agent ID is required to derive the default credential file path")
	}
}
```

- [ ] **Step 2: Add failing discovery tests**

Create `internal/config/agent_credential_test.go` with package `config` and
cover zero, one, multiple, and legacy-shared candidates:

```go
func TestDiscoverAgentCredentialFile(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		got, err := discoverAgentCredentialFile(t.TempDir())
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("one ID-scoped credential", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "agent-a", "credential.json")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))

		got, err := discoverAgentCredentialFile(root)
		require.NoError(t, err)
		assert.Equal(t, path, got)
	})

	t.Run("legacy shared credential is ignored", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "credential.json"), []byte("{}"), 0o600))

		got, err := discoverAgentCredentialFile(root)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("multiple credentials are ambiguous", func(t *testing.T) {
		root := t.TempDir()
		for _, id := range []string{"agent-a", "agent-b"} {
			path := filepath.Join(root, id, "credential.json")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
		}

		_, err := discoverAgentCredentialFile(root)
		require.EqualError(t, err, "multiple default agent credential files found; set --id or --credential-file")
	})
}
```

- [ ] **Step 3: Run the tests and verify RED**

Run:

```bash
go test ./internal/config -run 'TestDefaultAgentCredentialFile|TestDiscoverAgentCredentialFile' -count=1 -v
```

Expected: FAIL because empty IDs still return the shared path and
`discoverAgentCredentialFile` does not exist.

- [ ] **Step 4: Implement ID-only derivation and deterministic discovery**

Update `DefaultAgentCredentialFile` and add the two discovery helpers:

```go
func DefaultAgentCredentialFile(id string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("agent ID is required to derive the default credential file path")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for default credential file: %w", err)
	}
	return filepath.Join(home, ".unified-cd", id, "credential.json"), nil
}

func DiscoverDefaultAgentCredentialFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for default credential discovery: %w", err)
	}
	return discoverAgentCredentialFile(filepath.Join(home, ".unified-cd"))
}

func discoverAgentCredentialFile(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read default agent credential directory: %w", err)
	}
	var candidates []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), "credential.json")
		if _, err := os.Stat(path); err == nil {
			candidates = append(candidates, path)
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect default agent credential file: %w", err)
		}
	}
	switch len(candidates) {
	case 0:
		return "", nil
	case 1:
		return candidates[0], nil
	default:
		return "", fmt.Errorf("multiple default agent credential files found; set --id or --credential-file")
	}
}
```

- [ ] **Step 5: Run the tests and verify GREEN**

Run:

```bash
go test ./internal/config -run 'TestDefaultAgentCredentialFile|TestDiscoverAgentCredentialFile' -count=1 -v
```

Expected: PASS.

- [ ] **Step 6: Commit Task 1**

```bash
git add internal/config/agent.go internal/config/config_test.go internal/config/agent_credential_test.go
git commit -m "fix(agent): restore ID-scoped credential discovery"
```

---

### Task 2: Token-First Credential Resolution

**Files:**
- Modify: `internal/agent/credentials.go`
- Modify: `internal/agent/credentials_test.go`
- Modify: `cmd/unified-cd-agent/main.go`

**Interfaces:**
- Consumes: `config.DefaultAgentCredentialFile(string) (string, error)`.
- Consumes: `config.DiscoverDefaultAgentCredentialFile() (string, error)`.
- Produces: `CredentialManagerConfig.DefaultCredentialFile func(string) (string, error)`.
- Produces: `CredentialManagerConfig.DiscoverCredentialFile func() (string, error)`.
- Preserves: `EnsureIdentity(ctx context.Context) (string, error)`.

- [ ] **Step 1: Add a failing token-authority and destination test**

Add a test that gives the manager an old discoverable credential for
`agent-old` and a valid enrollment response for `agent-new`. Inject path
functions so the test does not use the real home directory:

```go
func TestCredentialManager_TokenFirstUsesReturnedIDPath(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	root := t.TempDir()
	oldPath := filepath.Join(root, "agent-old", "credential.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(oldPath), 0o700))
	require.NoError(t, writeCredentialFile(oldPath, persistedCredential{
		Version: 1, AgentID: "agent-old", RefreshToken: "ucr_old",
		RefreshExpiresAt: now.Add(time.Hour),
	}))
	newPath := filepath.Join(root, "agent-new", "credential.json")
	var discoveryCalls int
	srv := credentialServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/agents/enroll", r.URL.Path)
		response := tokenResponse("uca_new", "ucr_new", now)
		response.AgentID = "agent-new"
		_ = json.NewEncoder(w).Encode(response)
	})
	defer srv.Close()

	m := NewCredentialManager(CredentialManagerConfig{
		Server: srv.URL, EnrollmentToken: "uce_new", HTTPClient: srv.Client(),
		Now: func() time.Time { return now }, Jitter: func() time.Duration { return 0 },
		DefaultCredentialFile: func(id string) (string, error) {
			return filepath.Join(root, id, "credential.json"), nil
		},
		DiscoverCredentialFile: func() (string, error) {
			discoveryCalls++
			return oldPath, nil
		},
	})

	id, err := m.EnsureIdentity(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "agent-new", id)
	assert.Zero(t, discoveryCalls)
	_, err = readCredentialFile(newPath)
	require.NoError(t, err)
	_, err = readCredentialFile(oldPath)
	require.NoError(t, err)
}
```

- [ ] **Step 2: Add failing discovery and 401-fallback tests**

Add:

```go
func TestCredentialManager_EnsureIdentityDiscoversOneCredential(t *testing.T)
func TestCredentialManager_EnsureIdentityRejectsAmbiguousCredentials(t *testing.T)
func TestCredentialManager_Fallback401DiscoversOneCredential(t *testing.T)
func TestCredentialManager_Fallback401RejectsAmbiguousCredentials(t *testing.T)
```

Use injected discovery functions. The single-candidate tests return a real
temporary credential path. The ambiguous tests return:

```go
fmt.Errorf("multiple default agent credential files found; set --id or --credential-file")
```

Assert that token-less `EnsureIdentity` adopts the single credential offline,
401 fallback calls `/enroll` before discovery and then `/refresh`, and ambiguity
does not call `/refresh`.

- [ ] **Step 3: Run the focused tests and verify RED**

Run:

```bash
go test ./internal/agent -run 'TestCredentialManager_(TokenFirstUsesReturnedIDPath|EnsureIdentityDiscovers|EnsureIdentityRejectsAmbiguous|Fallback401Discovers|Fallback401RejectsAmbiguous)' -count=1 -v
```

Expected: FAIL to compile because the two resolver fields do not exist; after
only adding fields, the behavior tests still fail because `EnsureIdentity`
loads the old credential before enrollment.

- [ ] **Step 4: Separate configured and effective identity**

In `CredentialManager`, retain the explicitly configured ID separately:

```go
type CredentialManager struct {
	configuredAgentID string
	agentID           string
	// existing fields...
	defaultCredentialFile  func(string) (string, error)
	discoverCredentialFile func() (string, error)
}
```

Initialize the resolver dependencies to the `internal/config` functions when
the config fields are nil. Enrollment response validation must compare only
against `configuredAgentID`; an ID adopted from a local credential is not an
explicit assertion.

- [ ] **Step 5: Make explicit enrollment token resolution run before discovery**

Change `Token` and `EnsureIdentity` so the first explicit enrollment token is
exchanged before `loadRefreshCredential`. On success, validate the response,
adopt its ID, derive its destination, create the ID directory with mode `0700`,
and call the existing atomic persistence function.

Use a focused helper:

```go
func (m *CredentialManager) ensureCredentialFile(agentID string) error {
	if m.credentialFile != "" {
		return nil
	}
	path, err := m.defaultCredentialFile(agentID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	m.credentialFile = path
	return nil
}
```

For token-less resolution or HTTP 401 fallback, make
`loadRefreshCredential` select an explicit-ID default or call
`discoverCredentialFile` only when `credentialFile` is empty.

- [ ] **Step 6: Preserve safe fallback semantics**

On HTTP 401:

1. attempt local selection;
2. refresh only when one valid credential was loaded;
3. return the discovery ambiguity error unchanged;
4. return the original credential request error when no local credential
   exists; and
5. retain the current no-fallback behavior for 403, 429, 5xx, and network
   errors.

Do not add any deletion, rename, or cleanup of other credential paths.

- [ ] **Step 7: Remove eager shared-path setup from the host-agent entrypoint**

In `cmd/unified-cd-agent/main.go`:

- remove the `filepath` import;
- remove the block that calls `DefaultAgentCredentialFile(*id)` before creating
  `CredentialManager`;
- remove the eager `credentialExists` guard;
- pass the explicit `CredentialFile` value unchanged; and
- rely on `EnsureIdentity` for missing, ambiguous, enrollment, and persistence
  errors.

The manager's default resolver functions supply production path behavior, so
the entrypoint does not pass test hooks.

- [ ] **Step 8: Run focused and package tests and verify GREEN**

Run:

```bash
go test ./internal/agent -run 'TestCredentialManager_(EnrollFirst|Fallback401|No503Fallback|AdoptsAgentID|EnsureIdentity|AssertsConfiguredID|EnrollOnce|TokenFirstUsesReturnedIDPath)' -count=1 -v
go test ./internal/config ./internal/agent -count=1
go test ./cmd/unified-cd-agent -count=1
```

Expected: the focused tests PASS. If the pre-existing
`TestRunStepWithShell_CredentialsNotInheritedByChild` Windows environment
failure recurs in the package run, record it and rerun the credential-focused
tests to prove the changed surface is green.

- [ ] **Step 9: Commit Task 2**

```bash
git add internal/agent/credentials.go internal/agent/credentials_test.go cmd/unified-cd-agent/main.go
git commit -m "fix(agent): let enrollment token select credential path"
```

---

### Task 3: Breaking-Change Documentation

**Files:**
- Modify: `README.md`
- Modify: `docs/agents.md`
- Modify: `docs/cli.md`
- Modify: `docs/configuration.md`
- Modify: `docs/troubleshooting.md`
- Create: `docs/migration-agent-id-scoped-credentials.md`

**Interfaces:**
- Documents: token-first identity precedence and ID-scoped persistence.
- Documents: exact ambiguity error and explicit selection recovery.
- Documents: legacy shared-path migration without printing refresh tokens.

- [ ] **Step 1: Search all user-facing guidance**

Run:

```bash
rg -n 'shared.*credential|\\.unified-cd/credential\\.json|credentialFile|--credential-file|multiple agents' README.md docs examples templates --glob '!docs/superpowers/**'
```

Record every current-behavior statement that must change. Do not edit generated
files.

- [ ] **Step 2: Update enrollment and configuration documentation**

Document:

- a valid explicit enrollment token supplies the effective ID before local
  discovery;
- the default is always
  `$HOME/.unified-cd/<agent-id>/credential.json`;
- token-less ID-less startup discovers exactly one credential;
- multiple credentials require `--id` or `--credential-file`;
- the legacy shared path is ignored unless explicitly selected; and
- no process removes another ID's credential.

- [ ] **Step 3: Add the migration guide**

Create `docs/migration-agent-id-scoped-credentials.md` with:

- a before/after table;
- the exact error
  `multiple default agent credential files found; set --id or --credential-file`;
- POSIX and PowerShell commands that read only `.agentId`, create an owner-only
  destination directory, and move the credential without displaying
  `.refreshToken`;
- rollback instructions using explicit `--credential-file`; and
- a warning to stop the affected agent before moving its credential.

- [ ] **Step 4: Verify documentation consistency**

Run:

```bash
rg -n 'shared.*credential|\\.unified-cd/credential\\.json' README.md docs examples templates --glob '!docs/superpowers/**'
rg -n 'multiple default agent credential files found|\\.unified-cd/<.*>/credential\\.json' README.md docs
git diff --check
```

Expected: references to the shared path appear only in migration or explicit
legacy-compatibility guidance; no whitespace errors.

- [ ] **Step 5: Commit Task 3**

```bash
git add README.md docs/agents.md docs/cli.md docs/configuration.md docs/troubleshooting.md docs/migration-agent-id-scoped-credentials.md
git commit -m "docs: migrate agents to ID-scoped credentials"
```

---

### Task 4: Full Verification

**Files:**
- Verify only; modify files only if a test exposes an in-scope defect.

**Interfaces:**
- Verifies all outputs from Tasks 1-3.

- [ ] **Step 1: Format changed Go files**

Run:

```bash
gofmt -w internal/config/agent.go internal/config/config_test.go internal/config/agent_credential_test.go internal/agent/credentials.go internal/agent/credentials_test.go cmd/unified-cd-agent/main.go
```

- [ ] **Step 2: Run targeted regression tests**

Run:

```bash
go test ./internal/config -run 'TestDefaultAgentCredentialFile|TestDiscoverAgentCredentialFile' -count=1
go test ./internal/agent -run 'TestCredentialManager_(EnrollFirst|Fallback401|No503Fallback|AdoptsAgentID|EnsureIdentity|AssertsConfiguredID|EnrollOnce|TokenFirstUsesReturnedIDPath)' -count=1
```

Expected: PASS.

- [ ] **Step 3: Run the full Go test suite**

Run:

```bash
go test ./...
```

Expected: PASS, except that the already observed baseline-only Windows failure
`TestRunStepWithShell_CredentialsNotInheritedByChild` must be reported
separately if it recurs unchanged.

- [ ] **Step 4: Build all Go commands**

Run:

```bash
go build ./...
```

Expected: PASS.

- [ ] **Step 5: Inspect the final diff**

Run:

```bash
git diff --check
git status --short
git log --oneline --decorate -5
```

Expected: no uncommitted production or documentation changes and no unrelated
files.

---

## Plan Self-Review

**Spec coverage:** Token authority and deferred persistence are Task 2 Steps
1, 4, and 5. ID-only paths and legacy-path exclusion are Task 1. Unambiguous
token-less startup and 401 fallback are Task 2 Steps 2 and 6. Explicit ID and
credential-file behavior are preserved in Task 2. Multi-process
non-interference is enforced by the no-cleanup rule in Task 2 Step 6.
Breaking-change and troubleshooting guidance are Task 3. Verification is Task
4.

**Placeholder scan:** Every task contains concrete implementation and test
steps.

**Type consistency:** `DefaultAgentCredentialFile` and
`DiscoverDefaultAgentCredentialFile` are produced by Task 1 and consumed as the
constructor defaults for `CredentialManagerConfig.DefaultCredentialFile` and
`CredentialManagerConfig.DiscoverCredentialFile` in Task 2. `EnsureIdentity`
keeps its existing signature.
