# PR B — Label re-enrollment fix + inline enrollment token — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** (B1) Make re-enrollment with a freshly minted token actually apply the token's new labels. (B2) Let the enrollment token be passed to the agent inline (flag/env/stdin), not only via a file, and make `enrollment create` output directly usable by the agent.

**Architecture:** B1 is a localized store fix in `ConsumeAgentEnrollment` (only Postgres implements it). B2 threads an inline token value from the agent CLI (`cmd/unified-cd-agent`) through config into `internal/agent.CredentialManager`, and enriches `unified-cli agent enrollment create` output.

**Tech Stack:** Go 1.26.2, pgx/Postgres, cobra CLI.

## Global Constraints

- Module path `github.com/eirueimi/unified-cd`. No cgo/gcc → tests without `-race`.
- **B is labels-only for the re-enroll fix.** Do NOT also update `authorized_capabilities` on re-enroll — PR D removes admin-set enrollment capabilities entirely, so fixing capabilities-on-reenroll here would be throwaway work. (Rationale documented in the commit.)
- Conflict rule (decided): if BOTH an inline enrollment-token value and a token file resolve to non-empty at agent startup, fail with a clear error.
- Store tests need Postgres (`store.NewTestPostgres`); may be `[setup failed]`-flaky on first run — rerun once if so.
- Commit trailer: `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.
- Repo root `C:\Users\arimax\unified-cd-project\unified-cd`; prefix Bash with `cd … && …`.

---

### Task 1: B1 — re-enrollment applies the token's labels

**Files:**
- Modify: `internal/store/postgres_agent_auth.go` (`ConsumeAgentEnrollment`)
- Test: `internal/store/postgres_agent_auth_test.go` (or the existing enrollment test file)

**Interfaces:**
- Consumes: `agent_enrollment_tokens.authorized_labels` (already read into `labels` at ~line 165).
- Produces: on re-enroll of an existing identity, `agent_identities.authorized_labels` is updated to the token's labels and reflected in the returned `*AgentIdentity`.

- [ ] **Step 1: Write the failing store test (RED)**

Add to the store test suite (Postgres). It enrolls once with `label:a`, then re-enrolls (a second token) with `label:b`, and asserts the identity's labels became `label:b`:

```go
func TestConsumeAgentEnrollment_ReenrollUpdatesLabels(t *testing.T) {
	st := NewTestPostgres(t)
	ctx := context.Background()

	// First enrollment token with label a.
	tok1 := mustCreateEnrollmentToken(t, st, "vm-agent-01", []string{"kind:a"}, nil)
	id1, err := st.ConsumeAgentEnrollment(ctx, tok1.ID, tok1.hash, sampleIssue("enrollment"))
	require.NoError(t, err)
	require.Equal(t, []string{"kind:a"}, id1.AuthorizedLabels)

	// Second token for the same agent with label b; re-enroll.
	tok2 := mustCreateEnrollmentToken(t, st, "vm-agent-01", []string{"kind:b"}, nil)
	id2, err := st.ConsumeAgentEnrollment(ctx, tok2.ID, tok2.hash, sampleIssue("enrollment"))
	require.NoError(t, err)
	require.Equal(t, []string{"kind:b"}, id2.AuthorizedLabels, "re-enrollment must apply the new token's labels")

	// And the persisted identity reflects it.
	got, err := st.GetAgentIdentity(ctx, "vm-agent-01")
	require.NoError(t, err)
	require.Equal(t, []string{"kind:b"}, got.AuthorizedLabels)
}
```
(Use/adapt the existing test helpers for creating enrollment tokens + issue; if none exist, mirror the setup already used by the current `ConsumeAgentEnrollment` tests in this file. Match the real helper names.)

- [ ] **Step 2: Run it — expect FAIL (RED)**

Run: `go test ./internal/store/ -run TestConsumeAgentEnrollment_ReenrollUpdatesLabels -count=1 -v`
Expected: FAIL — `id2.AuthorizedLabels` is still `kind:a` (the bug). (If Postgres setup is flaky with `[setup failed]`, rerun once.)

- [ ] **Step 3: Fix `ConsumeAgentEnrollment` (GREEN)**

In `internal/store/postgres_agent_auth.go`, in the existing-identity branch (the `} else if identity.Status == "disabled" …` chain around line 191-195), add a final `else` that updates the labels to the token's:

```go
	} else if identity.EnrollmentMethod != issue.EnrollmentMethod || identity.ExternalSubject != issue.ExternalSubject {
		return nil, ErrAgentEnrollmentInvalid
	} else {
		// Re-enrollment: a freshly minted token can carry changed authorized
		// labels (an admin re-issues a token to change an agent's labels). Apply
		// them to the existing identity — otherwise the change is silently
		// ignored. (Capabilities are handled by PR D, which removes admin-set
		// enrollment capabilities; do not update them here.)
		if _, err := tx.Exec(ctx, `UPDATE agent_identities SET authorized_labels = $2 WHERE id = $1`,
			identity.ID, nonNilStrings(labels)); err != nil {
			return nil, fmt.Errorf("consume agent enrollment: update labels: %w", err)
		}
		identity.AuthorizedLabels = labels
	}
```

- [ ] **Step 4: Run it — expect PASS (GREEN)**

Run: `go test ./internal/store/ -run TestConsumeAgentEnrollment_ReenrollUpdatesLabels -count=1 -v`
Expected: PASS. Then run the whole store enrollment suite to catch regressions:
Run: `go test ./internal/store/ -run 'ConsumeAgentEnrollment|AgentIdentity|AgentEnrollment' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/postgres_agent_auth.go internal/store/*_test.go
git commit -m "$(printf 'fix(enrollment): re-enrollment applies the new token authorized labels\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

### Task 2: B2a — agent accepts the enrollment token inline (flag/env/stdin)

**Files:**
- Modify: `internal/agent/credentials.go` (`CredentialManagerConfig`, `CredentialManager`, enroll path)
- Modify: `internal/config/agent.go` (env `UNIFIED_AGENT_ENROLLMENT_TOKEN` → effective config)
- Modify: `cmd/unified-cd-agent/main.go` (`--enrollment-token` flag, stdin `-`, conflict check, wiring)
- Test: `internal/agent/credentials_test.go`

**Interfaces:**
- Consumes: the resolved enrollment-token value (flag/env/stdin) from `main.go`.
- Produces: `CredentialManagerConfig.EnrollmentToken string`; when set, the enroll exchange uses it instead of reading `EnrollmentTokenFile`.

- [ ] **Step 1: Add the inline value to CredentialManager (with test, RED first)**

Add a test to `internal/agent/credentials_test.go` that enrolls using an inline `EnrollmentToken` (no file) and asserts the enroll exchange succeeds and persists the refresh credential. Mirror the existing enrollment test (`credentials_test.go:45`) but pass `EnrollmentToken: "<token>"` instead of `EnrollmentTokenFile`.

Run it first — expect FAIL/compile-error (field doesn't exist yet).

- [ ] **Step 2: Implement the inline value (GREEN)**

In `internal/agent/credentials.go`:
- Add `EnrollmentToken string` to `CredentialManagerConfig` (after `EnrollmentTokenFile`).
- Add `enrollmentToken string` to `CredentialManager` and set it in `NewCredentialManager` (`enrollmentToken: cfg.EnrollmentToken`).
- In the enroll branch (~line 127), prefer the inline value:

```go
	} else {
		enrollment := m.enrollmentToken
		if enrollment == "" {
			var readErr error
			enrollment, readErr = readSecretFile(m.enrollmentTokenFile)
			if readErr != nil {
				return "", readErr
			}
		}
		response, err = m.exchangeWithRetry(ctx, "/api/v1/agents/enroll", strings.TrimSpace(enrollment))
	}
```
(Keep the existing `strings.TrimSpace` behavior if `readSecretFile` already trims; match current handling — do not double-trim inconsistently.)

Run the new test — expect PASS.

- [ ] **Step 3: Env + flag + stdin + conflict in `cmd/unified-cd-agent/main.go`**

- `internal/config/agent.go`: in `AgentEffective`, read `eff.EnrollmentToken = os.Getenv("UNIFIED_AGENT_ENROLLMENT_TOKEN")` (add an `EnrollmentToken string` field to the effective `AgentConfig` struct; do NOT add it as a persisted YAML file field — it is a one-time secret).
- `cmd/unified-cd-agent/main.go`: add
  ```go
  enrollmentToken := flag.String("enrollment-token", eff.EnrollmentToken, "one-time enrollment token value; use - to read from stdin (env: UNIFIED_AGENT_ENROLLMENT_TOKEN)")
  ```
  After flag parse, resolve stdin and the conflict:
  ```go
  tokenValue := *enrollmentToken
  if tokenValue == "-" {
      b, err := io.ReadAll(os.Stdin)
      if err != nil { slog.Error("read enrollment token from stdin", "error", err); os.Exit(1) }
      tokenValue = strings.TrimSpace(string(b))
  }
  if tokenValue != "" && *enrollmentTokenFile != "" {
      slog.Error("specify only one of --enrollment-token / UNIFIED_AGENT_ENROLLMENT_TOKEN or --enrollment-token-file")
      os.Exit(1)
  }
  ```
  Then pass `EnrollmentToken: tokenValue` into `CredentialManagerConfig` (alongside the existing `EnrollmentTokenFile`). The `!credentialExists && (*credentialFile == "" || *enrollmentTokenFile == "")` startup guard must also accept a non-empty `tokenValue` as satisfying the enrollment-token requirement — update it to `(*enrollmentTokenFile == "" && tokenValue == "")`.

- [ ] **Step 4: Build + tests**

Run:
```bash
go build ./...
go test ./internal/agent/ -run 'Credential' -count=1
go test ./internal/config/ -count=1
```
Expected: build + tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/credentials.go internal/agent/credentials_test.go internal/config/agent.go cmd/unified-cd-agent/main.go
git commit -m "$(printf 'feat(agent): accept enrollment token inline via --enrollment-token/env/stdin\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

### Task 3: B2b — `enrollment create` output usable directly (--quiet + inline)

**Files:**
- Modify: `internal/cli/agent_enrollment.go` (`newAgentEnrollmentCreateCmd`, `nextAgentCommands`)
- Test: `internal/cli/agent_enrollment_test.go`
- Docs: `docs/cli.md`, `docs/agents.md`

**Interfaces:**
- Consumes: the created token value in the RunE.
- Produces: `nextAgentCommands(server, agentID, tokenFile, tokenValue string)`; a `--quiet` flag on `enrollment create`.

- [ ] **Step 1: Update the enrollment-output test (RED)**

In `internal/cli/agent_enrollment_test.go`, `TestAgentEnrollmentCreatePrintsNextAgentCommands`, the no-`--output-file` case (around the second block) should now assert the suggested command contains the **actual token inline** and the `--enrollment-token` flag, e.g.:
```go
	assert.Contains(t, s, "--enrollment-token "+token)
	assert.NotContains(t, s, "<path-to-token-file>")
```
(Keep the `--output-file` case asserting `--enrollment-token-file`.) Add a new test `TestAgentEnrollmentCreateQuietPrintsOnlyToken` asserting `--quiet` output equals the token plus a trailing newline and contains no prose (`assert.Equal(t, token+"\n", out.String())`).

- [ ] **Step 2: Run — expect FAIL (RED)**

Run: `go test ./internal/cli/ -run 'TestAgentEnrollmentCreate' -count=1 -v`
Expected: FAIL (inline token not yet emitted; `--quiet` flag doesn't exist).

- [ ] **Step 3: Add `--quiet` and inline embedding (GREEN)**

In `newAgentEnrollmentCreateCmd`:
- Add `var quiet bool` and flag `cmd.Flags().BoolVar(&quiet, "quiet", false, "print only the token (for piping to unified-cd-agent --enrollment-token -)")`, and `cmd.MarkFlagsMutuallyExclusive("quiet", "output-file")`.
- In RunE, after unmarshalling `result`, handle quiet first:
  ```go
  if quiet {
      fmt.Fprintln(cmd.OutOrStdout(), result.Token)
      return nil
  }
  ```
- Change the stdout branch to pass the token value:
  ```go
  fmt.Fprintf(cmd.OutOrStdout(), "Enrollment token created (shown only once):\n\n%s\n", result.Token)
  fmt.Fprint(cmd.OutOrStdout(), nextAgentCommands(cfg.Server, agentID, "", result.Token))
  ```
- Change the output-file branch call to `nextAgentCommands(cfg.Server, agentID, outputFile, "")`.

Update `nextAgentCommands` signature to `(server, agentID, tokenFile, tokenValue string)`:
```go
func nextAgentCommands(server, agentID, tokenFile, tokenValue string) string {
	var b strings.Builder
	b.WriteString("\n")
	switch {
	case tokenFile != "":
		b.WriteString("Next, on the agent host, run the agent:\n")
		fmt.Fprintf(&b, "  unified-cd-agent \\\n    --server %s \\\n    --id %s \\\n    --enrollment-token-file %s\n",
			server, agentID, tokenFile)
	case tokenValue != "":
		b.WriteString("Next, on the agent host, run the agent (the token is visible in shell history/ps — prefer --output-file for shared hosts):\n")
		fmt.Fprintf(&b, "  unified-cd-agent \\\n    --server %s \\\n    --id %s \\\n    --enrollment-token %s\n",
			server, agentID, tokenValue)
	default:
		b.WriteString("Save this token to a private file on the agent host, then run the agent:\n")
		fmt.Fprintf(&b, "  unified-cd-agent \\\n    --server %s \\\n    --id %s \\\n    --enrollment-token-file <path-to-token-file>\n",
			server, agentID)
	}
	fmt.Fprintf(&b, "\nThe credential file defaults to $HOME/.unified-cd/%s/credential.json.\n", agentID)
	return b.String()
}
```

- [ ] **Step 4: Run — expect PASS (GREEN)**

Run: `go test ./internal/cli/ -run 'TestAgentEnrollmentCreate' -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Docs**

- `docs/cli.md` (agent enrollment section): document `--enrollment-token <value>` / `UNIFIED_AGENT_ENROLLMENT_TOKEN` / `--enrollment-token -` (stdin) on the agent, the `--quiet` flag on `enrollment create`, and the conflict rule (value + file → error). Keep the file form as the more-secure default; note the inline token is visible in shell history/ps. Show the pipe example:
  ```
  unified-cli agent enrollment create --agent-id agent-1 --label kind:linux --quiet \
    | unified-cd-agent --server https://ci.example.com --id agent-1 --enrollment-token -
  ```
- `docs/agents.md`: in the enroll → run flow, mention the inline/stdin option alongside the file form.

- [ ] **Step 6: Full build + test + commit**

Run: `go build ./... && go test ./internal/cli/ -count=1`
Expected: pass.
```bash
git add internal/cli/agent_enrollment.go internal/cli/agent_enrollment_test.go docs/cli.md docs/agents.md
git commit -m "$(printf 'feat(cli): enrollment create --quiet + inline token in run hint\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Self-Review

**Spec coverage:** B1 label re-enroll fix → Task 1. B2 inline token (flag/env/stdin, conflict) → Task 2. B2 create output (inline embed + `--quiet`) → Task 3. Docs → Task 3 Step 5. ✓

**Placeholder scan:** All code is concrete. The store-test helper names ("mustCreateEnrollmentToken", "sampleIssue") must be matched to the real helpers already in the store test file — Task 1 Step 1 says so explicitly.

**Type consistency:** `nextAgentCommands` gains a 4th param `tokenValue`; both callers updated in Task 3 Step 3. `CredentialManagerConfig.EnrollmentToken` + `CredentialManager.enrollmentToken` + `config.AgentConfig.EnrollmentToken` all named consistently. B is labels-only (capabilities untouched) — consistent with PR D.

**Ordering:** Task 1 (store) independent; Task 2 (agent) independent; Task 3 (create) independent. Any order works, but 1→2→3 is logical.
