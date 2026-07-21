# Remove `unified-cli agent install` / `agent uninstall` — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the `agent install` and `agent uninstall` CLI subcommands (and their exclusive helpers), collapse the enrollment "next steps" output to the single direct-run path, and update docs — while keeping the agent's `-f` config-file layer, `config.DefaultAgentCredentialFile`, and the `list`/`get`/`runs`/`enrollment`/`identity` subcommands.

**Architecture:** `unified-cli agent` is a cobra command tree. `install`/`uninstall` live in `internal/cli/agent_install.go` alongside `list`/`get`/`runs`; they scaffold systemd/launchd service files and an `agent.yaml`. Removing them is a deletion of specific functions + their two registrations + exclusive helpers, plus a one-branch simplification of `nextAgentCommands` in `agent_enrollment.go`, plus test and doc updates.

**Tech Stack:** Go 1.26.2, cobra CLI, Markdown docs.

## Global Constraints

- **Scope A only:** remove the two CLI subcommands + their exclusive helpers. Do NOT touch `internal/config/agent.go` (the `-f` config-file layer / 18 fields), `config.DefaultAgentCredentialFile`, or `cmd/agent`'s default-credential behavior.
- **Keep** `agent list` / `get` / `runs`, `agent enrollment*`, `agent identity`, and their tests.
- **The CLI-side `AgentConfig` struct** (in `agent_install.go`) is distinct from `config.AgentFile`; only the former is removed.
- **No dangling references:** after the change, a repo-wide scan for the removed command/symbol names (excluding `docs/superpowers/**`, `vendor/`, `.superpowers/`) must be empty. A manual-service-example doc line uses `unified-cd-agent`, not `agent install`.
- Every commit message ends with the `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>` trailer.
- Run all commands from the repo root `C:\Users\arimax\unified-cd-project\unified-cd`. Prefix Bash commands with `cd /c/Users/arimax/unified-cd-project/unified-cd && ...` (the Bash working dir can reset between calls).
- This environment has no cgo/gcc: run Go tests without `-race`.

---

### Task 1: Remove the CLI commands, simplify enrollment output, fix tests

**Files:**
- Modify: `internal/cli/agent_install.go`
- Modify: `internal/cli/agent_enrollment.go` (`nextAgentCommands`)
- Modify: `internal/cli/agent_install_test.go` (remove install/uninstall tests, keep list/get/runs)
- Modify: `internal/cli/agent_enrollment_test.go` (`TestAgentEnrollmentCreatePrintsNextAgentCommands`)

**Interfaces:**
- Consumes: nothing new.
- Produces: `unified-cli agent` tree with no `install`/`uninstall`; `nextAgentCommands(server, agentID, tokenFile string) string` (the `labels []string` parameter is **removed**) now emits only the direct-run command, with **no `--labels` flag** — labels are bound at enrollment (`--label`) and the runtime `--labels` is ignored for enrolled agents.

- [ ] **Step 1: Update the enrollment-output test assertions first (RED)**

In `internal/cli/agent_enrollment_test.go`, `TestAgentEnrollmentCreatePrintsNextAgentCommands`:

Replace line ~65:
```go
	assert.Contains(t, s, "unified-cli agent install")
```
with:
```go
	assert.NotContains(t, s, "agent install")
```

Delete line ~70 AND line ~71 entirely — neither the install-style singular `--label` nor the runtime `--labels` flag appears in the output any more:
```go
	assert.Contains(t, s, "--label kind:linux")
	assert.Contains(t, s, "--labels kind:linux")
```
and add, in their place, a lock that no label flag is emitted:
```go
	assert.NotContains(t, s, "--label")
```
(`--label` is a prefix of `--labels`, so this single assertion rules out both.)

Replace line ~83:
```go
	assert.Contains(t, s, "unified-cli agent install")
```
with:
```go
	assert.NotContains(t, s, "agent install")
```

- [ ] **Step 2: Run the test to see it fail (RED)**

Run: `go test ./internal/cli/ -run TestAgentEnrollmentCreatePrintsNextAgentCommands -v`
Expected: FAIL — the current `nextAgentCommands` still prints `unified-cli agent install`, so `NotContains` fails.

- [ ] **Step 3: Simplify `nextAgentCommands` (GREEN)**

In `internal/cli/agent_enrollment.go`, replace the whole `nextAgentCommands` function (drop the `labels` parameter; keep the doc comment above it, updating its wording if it mentions install) with:

```go
func nextAgentCommands(server, agentID, tokenFile string) string {
	tokenRef := tokenFile
	var b strings.Builder
	b.WriteString("\n")
	if tokenRef == "" {
		tokenRef = "<path-to-token-file>"
		b.WriteString("Save this token to a private file on the agent host, then run the agent:\n")
	} else {
		b.WriteString("Next, on the agent host, run the agent:\n")
	}

	fmt.Fprintf(&b, "  unified-cd-agent \\\n    --server %s \\\n    --id %s \\\n    --enrollment-token-file %s\n",
		server, agentID, tokenRef)
	fmt.Fprintf(&b, "\nThe credential file defaults to $HOME/.unified-cd/%s/credential.json.\n", agentID)
	return b.String()
}
```

Then update the two callers in the same file (the `enrollment create` RunE, ~lines 163 and 167) to drop the now-removed `labels` argument:
```go
fmt.Fprint(cmd.OutOrStdout(), nextAgentCommands(cfg.Server, agentID, outputFile))
...
fmt.Fprint(cmd.OutOrStdout(), nextAgentCommands(cfg.Server, agentID, ""))
```
The local `labels` variable stays — it is still used to build the enrollment request (`Labels: labels`); it is simply no longer passed to `nextAgentCommands`. `go build ./internal/cli/` will flag it if a caller or the `labels`/`installLabels` locals are left dangling — remove any now-unused local.

- [ ] **Step 4: Run the test to see it pass (GREEN)**

Run: `go test ./internal/cli/ -run TestAgentEnrollmentCreatePrintsNextAgentCommands -v`
Expected: PASS.

- [ ] **Step 5: Remove the install/uninstall commands and registrations**

In `internal/cli/agent_install.go`:

Remove the two registration lines from `newAgentCmdWithClient` so it reads:
```go
func newAgentCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Agent management commands",
	}
	cmd.AddCommand(newAgentListCmd(resolve, httpClient))
	cmd.AddCommand(newAgentGetCmd(resolve, httpClient))
	cmd.AddCommand(newAgentRunsCmd(resolve, httpClient))
	cmd.AddCommand(newAgentEnrollmentCmd(resolve, httpClient))
	cmd.AddCommand(newAgentEnrollmentPolicyCmd(resolve, httpClient))
	cmd.AddCommand(newAgentIdentityCmd(resolve, httpClient))
	return cmd
}
```

Delete these functions/types entirely from the file:
- `AgentConfig` struct (the `yaml`-tagged CLI writer struct near the top of the file — NOT `config.AgentFile`).
- `newAgentInstallCmd`, `newAgentUninstallCmd`.
- `reportRemove`, `writeAgentConfig`, `generateSystemdUnit`, `generateLaunchdPlist`, `agentCredentialArgs`, `installLinux`, `installDarwin`.

Keep: `newAgentCmd`, `newAgentCmdWithClient`, `newAgentListCmd`, `newAgentGetCmd`, `newAgentRunsCmd`.

- [ ] **Step 6: Prune now-unused imports in `agent_install.go`**

After Step 5, the remaining code (list/get/runs) uses only: `context`, `encoding/json`, `fmt`, `io`, `net/http`, `strings`, `github.com/eirueimi/unified-cd/internal/api`, `github.com/spf13/cobra`. Remove the now-unused imports: `os`, `path/filepath`, `runtime`, `github.com/eirueimi/unified-cd/internal/config`, `gopkg.in/yaml.v3`.

Run: `go build ./internal/cli/`
Expected: compiles (a leftover unused import or symbol is a compile error here — fix until clean).

- [ ] **Step 7: Remove the install/uninstall tests**

In `internal/cli/agent_install_test.go`, delete these test functions (they exercise the removed code): `TestGenerateSystemdUnit`, `TestGenerateLaunchdPlist`, `TestWriteAgentConfig`, `TestNewAgentInstallCmd_FlagsExist`, `TestAgentInstallDefaultsCredentialFileWhenOmitted`, `TestAgentInstallRequiresEnrollmentForMissingCredentialFile`, `TestAgentUninstallRemovesGeneratedFiles`, `TestAgentUninstallReportsWhenNothingToRemove`, `TestAgentUninstallPurgeCredentialsRequiresID`, `TestAgentUninstallPurgeCredentialsRemovesDir`.

Keep: `TestAgentList_*`, `TestAgentGet_*`, `TestAgentRuns_*`.

Then prune any imports in that test file that are now unused (build will tell you). Run:
Run: `go vet ./internal/cli/`
Expected: no errors (catches unused imports/symbols in the test file).

- [ ] **Step 8: Run the CLI test suite**

Run: `go test ./internal/cli/ -run 'TestAgent' -count=1`
Expected: PASS (kept list/get/runs tests + the updated enrollment-output test).

- [ ] **Step 9: Full build + confirm the command is gone**

Run:
```bash
go build ./...
go build -o /tmp/unified-cli ./cmd/unified-cli
/tmp/unified-cli agent --help 2>&1 | grep -E "install|uninstall" || echo "NO-INSTALL-SUBCOMMAND"
/tmp/unified-cli agent install 2>&1 | head -2
```
Expected: `go build ./...` succeeds; the help grep prints `NO-INSTALL-SUBCOMMAND`; `agent install` errors as an unknown command (cobra "unknown command \"install\"").

- [ ] **Step 10: Commit**

```bash
git add internal/cli/agent_install.go internal/cli/agent_enrollment.go internal/cli/agent_install_test.go internal/cli/agent_enrollment_test.go
git commit -m "$(printf 'feat(cli): remove agent install/uninstall; enroll output shows direct-run only\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

### Task 2: Docs — drop install/uninstall, add a manual service example, scan clean

**Files:**
- Modify: `docs/cli.md`
- Modify: `docs/agents.md`
- Modify: `docs/getting-started.md` (only if it references the removed commands)

**Interfaces:**
- Consumes: the direct-run flow from Task 1.
- Produces: docs with no reference to the removed commands and a hand-written service recipe.

- [ ] **Step 1: Enumerate every live reference (checklist for this task)**

Run:
```bash
grep -rn "agent install\|agent uninstall\|purge-credentials\|generateSystemdUnit\|generateLaunchdPlist" \
  . --include=*.go --include=*.md --include=*.yaml --include=*.yml --include=*.toml \
  | grep -v "docs/superpowers/" | grep -v "vendor/" | grep -v "\.superpowers/"
```
This is the authoritative list. Every non-`superpowers` hit must be resolved in this task (Task 1 already handled the `.go` files; the remainder are docs). If a hit appears in a file this plan didn't name, handle it the same way and note it in the report.

- [ ] **Step 2: `docs/cli.md` — remove the install/uninstall sections**

Read `docs/cli.md` around the `agent install` and `agent uninstall` headings, then delete both sections (including PR #70's `--purge-credentials` / uninstall block and the install-scoped `--credential-file` default note). If the credential-file default is documented only under `install`, move a one-line statement of it to the agent-runtime context (it is a `unified-cd-agent --credential-file` / `-f` config default: `$HOME/.unified-cd/<id>/credential.json` when unset). Leave the `enrollment`, `list`, `get`, `runs`, `identity` sections intact.

- [ ] **Step 3: `docs/agents.md` — rewrite install mentions to direct-run**

Read the passages in `docs/agents.md` that mention `agent install`; rewrite them to the enroll → run-`unified-cd-agent`-directly flow (matching `nextAgentCommands`' new output). Remove any "install as a service" framing tied to the removed command.

- [ ] **Step 4: Add a manual service example**

In `docs/agents.md` (in/near the section on running the agent as a service), add a short **hand-written** service recipe so the removed command's convenience is preserved. Use these literal examples (derived from the former generators):

systemd (`~/.config/systemd/user/unified-cd-agent.service`):
```ini
[Unit]
Description=unified-cd Agent (agent-1)
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/unified-cd-agent --server=https://ci.example.com --id=agent-1 --enrollment-token-file=/var/lib/unified-cd-agent/enrollment.token
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```
Enable with:
```bash
systemctl --user daemon-reload
systemctl --user enable --now unified-cd-agent
```

launchd (`~/Library/LaunchAgents/dev.unified-cd.agent.plist`):
```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>dev.unified-cd.agent</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/unified-cd-agent</string>
    <string>--server=https://ci.example.com</string>
    <string>--id=agent-1</string>
    <string>--enrollment-token-file=/var/lib/unified-cd-agent/enrollment.token</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
</dict>
</plist>
```
Load with `launchctl load ~/Library/LaunchAgents/dev.unified-cd.agent.plist`.

Note in the prose that the credential file defaults to
`$HOME/.unified-cd/<id>/credential.json` when `--credential-file` is omitted.
Also note that an enrolled agent's **labels are fixed at enrollment**
(`unified-cli agent enrollment create --label …`); the runtime `--labels` flag
is honored only for legacy shared-token agents and is ignored for enrolled
agents, so the service command deliberately omits it.

- [ ] **Step 5: `docs/getting-started.md` — verify no install reference**

Run: `grep -n "agent install\|agent uninstall" docs/getting-started.md || echo "clean"`
If any hit exists, rewrite it to the direct-run flow; otherwise leave the file unchanged.

- [ ] **Step 6: Final repo-wide scan (must be clean)**

Run:
```bash
grep -rn "agent install\|agent uninstall\|purge-credentials\|generateSystemdUnit\|generateLaunchdPlist\|newAgentInstallCmd\|newAgentUninstallCmd\|writeAgentConfig\|reportRemove" \
  . --include=*.go --include=*.md --include=*.yaml --include=*.yml --include=*.toml \
  | grep -v "docs/superpowers/" | grep -v "vendor/" | grep -v "\.superpowers/"
```
Expected: **no output.** (The manual-service example uses `unified-cd-agent`, which does not match.)

- [ ] **Step 7: Commit**

```bash
git add docs/cli.md docs/agents.md docs/getting-started.md
git commit -m "$(printf 'docs: drop agent install/uninstall; add manual systemd/launchd example\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Self-Review

**Spec coverage:**
- Scope A: remove CLI commands + exclusive helpers → Task 1 Steps 5-6. ✓
- Enrollment output → single direct-run → Task 1 Steps 1-4. ✓
- Tests (remove install/uninstall, update enrollment) → Task 1 Steps 1, 7-8. ✓
- Docs (cli.md/agents.md/getting-started) + manual service example → Task 2. ✓
- Keep `-f` config layer / `DefaultAgentCredentialFile` / list/get/runs → Global Constraints + not touched. ✓
- Mandatory repo-wide reference scan → Task 2 Steps 1 & 6. ✓

**Placeholder scan:** All code, test edits, commands, and the manual-example text are literal. No TBD/"handle appropriately".

**Type consistency:** `nextAgentCommands` signature changes to `(server, agentID, tokenFile string) string` (the `labels []string` param is dropped); both callers in `agent_enrollment.go` are updated in the same step, and the enrollment-output test no longer asserts any label flag. Removed symbols (`AgentConfig`, `writeAgentConfig`, `generateSystemdUnit`, `generateLaunchdPlist`, `installLinux`, `installDarwin`, `agentCredentialArgs`, `reportRemove`, `newAgentInstallCmd`, `newAgentUninstallCmd`) are all defined only in `agent_install.go` and referenced only there + in the removed tests — no external consumer.

**Ordering:** Task 1 before Task 2 (Task 2's Step 1 grep expects the `.go` references already gone).
