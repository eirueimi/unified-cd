# Remove `unified-cli agent install` / `agent uninstall`

**Date:** 2026-07-21
**Status:** Approved (brainstorming)

## Motivation

`unified-cli agent install` and `agent uninstall` are convenience commands that
scaffold a system service around the agent: `install` writes an `agent.yaml`
and a systemd unit / launchd plist (and prints enable instructions);
`uninstall` removes those generated files and optionally purges the credential
directory. Having them makes agent startup confusing — there are now two
advertised paths (install-as-a-service vs. run-directly), and the enrollment
flow prints both. The startup story should be a single path: **enroll → run the
agent directly** (flags / env / the agent's `-f` config file).

## Scope (decided)

**Remove only the two CLI subcommands and their exclusive helpers.** Keep
everything the agent runtime depends on:

- **Keep** the agent's `-f` config-file layer (`config.AgentFile` /
  `LoadAgent` / `AgentEffective`, default `unified-agent.yaml`). It is a
  first-class, 18-field configuration mechanism (server, token, credentialFile,
  enrollmentTokenFile, id, labels, exposeEnv, cache*, maxConcurrent,
  cleanWorkspace, workspaceDir, drainTimeout, pauseImage, runnerImage,
  minFreeDisk, workspaceRetentionDays), independent of the install command.
- **Keep** `config.DefaultAgentCredentialFile` — the agent runtime
  (`cmd/agent`) uses it to default the credential path, not just `install`.
- **Keep** the agent-management subcommands `agent list` / `get` / `runs` and
  `agent enrollment` / `enrollment-policy` / `identity`.

Service guidance is not dropped: docs gain a **short manual systemd/launchd
example** so users can still run the agent as a service themselves.

This was never in a tagged release as-expanded (PR #70 added `uninstall` and
expanded `install` earlier today), so no deprecation shim is needed — remove
outright.

## Changes

### 1. CLI removal — `internal/cli/agent_install.go`

Delete:
- `newAgentInstallCmd()` and `newAgentUninstallCmd()`.
- Their two `cmd.AddCommand(...)` registrations in `newAgentCmdWithClient`.
- Exclusive helpers used only by install/uninstall:
  `AgentConfig` (the 7-field CLI writer struct — distinct from
  `config.AgentFile`), `writeAgentConfig`, `generateSystemdUnit`,
  `generateLaunchdPlist`, `installLinux`, `installDarwin`,
  `agentCredentialArgs`, `reportRemove`, and the `LabelsString` method on the
  CLI `AgentConfig` if unused elsewhere.
- Now-unused imports (likely `config`, `os`, `path/filepath`, `runtime`,
  `gopkg.in/yaml.v3` — verify by compiling; `goimports`/build will confirm).

Keep in this file: `newAgentCmd`, `newAgentCmdWithClient` (minus the two
removed registrations), `newAgentListCmd`, `newAgentGetCmd`, `newAgentRunsCmd`.
(The filename stays `agent_install.go` for minimal churn even though it no
longer defines install; renaming is out of scope.)

### 2. Enrollment output — `internal/cli/agent_enrollment.go` `nextAgentCommands`

Currently prints two options ("install the agent as a service:
`unified-cli agent install …`" **or** "run the agent directly:
`unified-cd-agent …`"). Collapse to the single direct-run path:

- Drop the "install the agent as a service" sentence and the
  `unified-cli agent install …` block, and the `installLabels` builder.
- Keep and lead with the direct-run block (`unified-cd-agent --server … --id …
  --enrollment-token-file …`) and the trailing
  "The credential file defaults to $HOME/.unified-cd/<id>/credential.json."
- **Drop `--labels` from the direct-run command** and remove the `labels`
  parameter from `nextAgentCommands` entirely. Rationale (verified): the server
  overrides an enrolled agent's advertised labels with the enrollment-bound
  `AuthorizedLabels` (`api_agent.go`: `if principal.AuthMethod != "legacy" {
  labels = principal.AuthorizedLabels }`); the runtime `--labels` flag is
  honored only for legacy shared-token agents, so showing it for the enrollment
  (modern) flow is misleading. Labels are set at enrollment via `--label`.
- Simplify the intro wording (no "either … or …"): e.g. when no token file is
  known, "Save this token to a private file on the agent host, then run the
  agent:"; otherwise "Next, on the agent host, run the agent:".

### 3. Tests

- `internal/cli/agent_install_test.go`: remove the install/uninstall tests —
  `TestGenerateSystemdUnit`, `TestGenerateLaunchdPlist`, `TestWriteAgentConfig`,
  `TestNewAgentInstallCmd_FlagsExist`,
  `TestAgentInstallDefaultsCredentialFileWhenOmitted`,
  `TestAgentInstallRequiresEnrollmentForMissingCredentialFile`,
  `TestAgentUninstallRemovesGeneratedFiles`,
  `TestAgentUninstallReportsWhenNothingToRemove`,
  `TestAgentUninstallPurgeCredentialsRequiresID`,
  `TestAgentUninstallPurgeCredentialsRemovesDir`.
  **Keep** the agent-management tests in the same file: `TestAgentList_*`,
  `TestAgentGet_*`, `TestAgentRuns_*`. Prune any now-unused test imports.
- `internal/cli/agent_enrollment_test.go`: the two assertions at ~lines 65 and
  83 that `assert.Contains(s, "unified-cli agent install")` must change to
  assert the direct-run command is present instead (e.g.
  `assert.Contains(s, "unified-cd-agent")`) and that the install line is
  **absent** (`assert.NotContains(s, "agent install")`) — locking in the
  single-path behavior.

### 4. Docs

Update every doc that references the removed commands (see the mandatory
repo-wide scan below — do not rely on this list being complete):

- `docs/cli.md`: delete the `agent install` and `agent uninstall` sections
  (including PR #70's `--purge-credentials` / uninstall block and the
  install `--credential-file` default note). Ensure the credential-file
  default is still documented where it belongs — on the **agent runtime**
  (`unified-cd-agent --credential-file` / `-f` config), not under a removed
  command.
- `docs/agents.md`: rewrite the passages that mention `agent install` to the
  direct-run flow.
- **Manual service example (decided):** add a short section (in `docs/agents.md`
  or `docs/cli.md`, wherever service setup is discussed) giving a minimal
  hand-written systemd unit and launchd plist that runs
  `unified-cd-agent --server … --id … --enrollment-token-file …`, so the
  service recipe survives the command's removal. Reuse the shapes from the
  deleted `generateSystemdUnit` / `generateLaunchdPlist` as the example text.
- `docs/getting-started.md`: its §4 already uses enroll + direct-run; verify no
  install reference sneaks in.

### 5. Keep — do NOT touch

- `internal/config/agent.go` (the `-f` config-file layer and all 18 fields).
- `config.DefaultAgentCredentialFile` and `cmd/agent`'s default-credential
  behavior (PR #70's default credential file feature stays).
- `agent list` / `get` / `runs`, `enrollment*`, `identity`.

## Verification

- **Mandatory repo-wide reference scan** (lesson from the prior task, whose
  too-narrow `docs/`-only grep missed live references): after the change,
  ```
  grep -rn "agent install\|agent uninstall\|generateSystemdUnit\|generateLaunchdPlist\|newAgentInstallCmd\|newAgentUninstallCmd\|writeAgentConfig\|reportRemove" \
    . --include=*.go --include=*.md --include=*.yaml --include=*.yml --include=*.toml \
    | grep -v "docs/superpowers/" | grep -v "vendor/" | grep -v "\.superpowers/"
  ```
  must return **nothing** (a manual-example doc line will say `unified-cd-agent`,
  not `agent install`, so it won't match).
- `go build ./...` succeeds (confirms no dangling references to removed
  symbols and no unused imports).
- `go test ./internal/cli/ -run 'TestAgent'` passes: the kept
  list/get/runs tests still pass, and the enrollment-output test asserts the
  single direct-run path.
- `unified-cli agent --help` no longer lists `install` / `uninstall`;
  `unified-cli agent install` errors as an unknown command.
- `unified-cli agent enrollment create …` output shows only the direct-run
  command (no `agent install`).

## Risks / notes

- Removing a CLI subcommand is a breaking change for anyone scripting
  `agent install`/`uninstall`. Accepted per the request; the commands were
  convenience scaffolding, and the direct-run path (with the manual service
  example) fully replaces them.
- The CLI-side `AgentConfig` struct being deleted is distinct from
  `config.AgentFile`; do not confuse them. Only the former is removed.
