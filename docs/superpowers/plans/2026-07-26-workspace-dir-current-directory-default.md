# Current-Directory Agent Workspace Default Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `unified-cd-agent` use its startup working directory as the workspace base when no workspace directory is configured.

**Architecture:** Add a single exported resolver in `internal/agent` that maps an empty value to `os.Getwd()` and otherwise preserves the existing `~/` expansion and relative-path behavior. Resolve the CLI's effective value once before shim installation, pass that same value into `Agent`, and keep defensive resolution inside package entry points for direct callers.

**Tech Stack:** Go 1.26.2, standard `flag` and `os/path/filepath` packages, Testify assertions, Markdown documentation.

## Global Constraints

- The unset default is the absolute process working directory captured during agent startup.
- Explicit `--workspace-dir`, `UNIFIED_AGENT_WORKSPACE_DIR`, and `workspaceDir` values keep their current precedence and path semantics.
- Existing `~/` expansion remains supported.
- `--clean-workspace`, detached cleanup, and workspace GC must never remove the workspace base or unrelated files directly beneath it.
- Existing workspaces under `~/workspace` are not migrated; operators can retain that location by configuring it explicitly.
- All repository code, comments, tests, documentation, and commit messages must be English.
- Live documentation, examples, and templates must agree with the new default.
- The pre-change Windows baseline has four environment-dependent failures: VCS safe-directory stamping, Windows `bash.exe` selection, and unavailable Unix `cat`/`ls` executables. Focused tests must pass; the final full-suite result must be compared with this recorded baseline and CI must pass.

---

## File Map

- `internal/agent/agent.go`: owns workspace-path resolution and uses it from `Agent.Run` and `InstallShim`.
- `internal/agent/install_shim_test.go`: covers the empty default, explicit relative paths, home expansion, and shim placement.
- `cmd/unified-cd-agent/main.go`: resolves the effective workspace once, installs the shim beneath it, assigns it to the agent, and describes the new default in help output.
- `docs/configuration.md`: documents the flag, environment variable, generated directories, compatibility, and migration behavior.
- `docs/agents.md`: documents the runtime layout and corrects the `.ucd-tools` location.
- `TODO.md`: removes the resolved `~/workspace` hard-coding entry.
- `examples/` and `templates/`: receive no mechanical change unless the required scan finds a live statement of the old default.

---

### Task 1: Resolve and Wire the Workspace Base

**Files:**
- Modify: `internal/agent/agent.go:223-231,701-710,719-770`
- Modify: `internal/agent/install_shim_test.go:77-93`
- Modify: `cmd/unified-cd-agent/main.go:76-80,112-135,217-223`

**Interfaces:**
- Produces: `agent.ResolveWorkspaceDir(workspaceDir string) (string, error)`.
- Consumes: `os.Getwd()`, existing `expandHome(string) (string, error)`, `InstallShim(string) (string, error)`, and `Agent.WorkspaceDir`.

- [ ] **Step 1: Replace the old shim-default test and add resolver contract tests**

In `internal/agent/install_shim_test.go`, replace
`TestInstallShim_DefaultsEmptyWorkspaceDir` and add these focused tests:

```go
func TestResolveWorkspaceDir_DefaultsToCurrentDirectory(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)

	got, err := ResolveWorkspaceDir("")
	require.NoError(t, err)
	assert.Equal(t, cwd, got)
	assert.True(t, filepath.IsAbs(got))
}

func TestResolveWorkspaceDir_PreservesExplicitRelativePath(t *testing.T) {
	got, err := ResolveWorkspaceDir(filepath.Join("relative", "workspace"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("relative", "workspace"), got)
}

func TestResolveWorkspaceDir_ExpandsHome(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	got, err := ResolveWorkspaceDir("~/workspace")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(fakeHome, "workspace"), got)
}

func TestInstallShim_DefaultsEmptyWorkspaceDirToCurrentDirectory(t *testing.T) {
	payload := []byte("fake-shim")
	withFakeShimBytes(t, payload)

	cwd := t.TempDir()
	t.Chdir(cwd)

	toolsDir, err := InstallShim("")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(cwd, ".ucd-tools"), toolsDir)
	require.FileExists(t, filepath.Join(toolsDir, "ucd-sh"))
}
```

Do not call `t.Parallel()` in tests that use `t.Chdir` or modify home-directory
environment variables.

- [ ] **Step 2: Run the focused tests and verify they fail for the old default**

Run:

```powershell
go test ./internal/agent -run 'TestResolveWorkspaceDir|TestInstallShim_DefaultsEmptyWorkspaceDir' -count=1
```

Expected: FAIL because `ResolveWorkspaceDir` is undefined and the old
`InstallShim("")` behavior targets `~/workspace`.

- [ ] **Step 3: Add the shared resolver and use it from both package entry points**

In `internal/agent/agent.go`, replace the duplicated empty-value fallbacks with:

```go
// ResolveWorkspaceDir resolves the workspace base used by the standard agent.
// An empty value means the process's current working directory. Explicit
// values retain the existing leading "~/" expansion and relative-path
// semantics.
func ResolveWorkspaceDir(workspaceDir string) (string, error) {
	if workspaceDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve default workspace-dir from current directory: %w", err)
		}
		return cwd, nil
	}
	return expandHome(workspaceDir)
}
```

At the start of `Agent.Run`'s workspace setup, use:

```go
wsBase, err := ResolveWorkspaceDir(a.WorkspaceDir)
if err != nil {
	return err
}
```

In `InstallShim`, use:

```go
wsBase, err := ResolveWorkspaceDir(workspaceDir)
if err != nil {
	return "", err
}
```

Update the `InstallShim` documentation comment so it says an empty value
defaults to the current directory, not `~/workspace`.

- [ ] **Step 4: Resolve the CLI value once and update the flag help**

In `cmd/unified-cd-agent/main.go`, change the flag description to:

```go
workspaceDir := flag.String("workspace-dir", eff.WorkspaceDir, "base directory for run workspaces (default: current directory at agent startup) (env: UNIFIED_AGENT_WORKSPACE_DIR)")
```

After `RequireShell` succeeds and before `InstallShim`, resolve the effective
value once:

```go
resolvedWorkspaceDir, err := agent.ResolveWorkspaceDir(*workspaceDir)
if err != nil {
	slog.Error("resolve workspace directory", "error", err)
	os.Exit(1)
}

toolsDir, err := agent.InstallShim(resolvedWorkspaceDir)
```

Assign that same value to the agent:

```go
a.WorkspaceDir = resolvedWorkspaceDir
```

This guarantees shim installation and run execution use the same absolute
startup directory when configuration is unset.

- [ ] **Step 5: Format and run focused verification**

Run:

```powershell
gofmt -w internal/agent/agent.go internal/agent/install_shim_test.go cmd/unified-cd-agent/main.go
go test ./internal/agent -run 'TestResolveWorkspaceDir|TestInstallShim_' -count=1
go test ./cmd/unified-cd-agent -count=1
go test ./internal/agent -run 'TestGCWorkspaces|TestPrepareWorkspace' -count=1
go build -buildvcs=false ./cmd/unified-cd-agent
```

Expected: every command passes. The cleanup tests demonstrate that changing
the base does not broaden deletion.

- [ ] **Step 6: Check the CLI help text**

Run:

```powershell
go run -buildvcs=false ./cmd/unified-cd-agent --help 2>&1 | Select-String 'workspace-dir'
```

Expected: the `workspace-dir` line contains
`default: current directory at agent startup` and does not contain
`default: ~/workspace`.

- [ ] **Step 7: Commit the code and tests**

```powershell
git add internal/agent/agent.go internal/agent/install_shim_test.go cmd/unified-cd-agent/main.go
git commit -m "feat(agent): default workspace to current directory"
```

---

### Task 2: Document the New Default and Verify Repository Consistency

**Files:**
- Modify: `docs/configuration.md:168,183-200`
- Modify: `docs/agents.md:433-441,509-519`
- Modify: `TODO.md:72-75`
- Inspect and modify only if matched: `examples/**`, `templates/**`

**Interfaces:**
- Consumes: `agent.ResolveWorkspaceDir(workspaceDir string) (string, error)` and the layout `.ucd-tools`, `working<N>`, and `detached`.
- Produces: user-facing upgrade guidance consistent with CLI behavior.

- [ ] **Step 1: Update configuration reference text**

In `docs/configuration.md`, make the flag and environment table entries read:

```text
--workspace-dir         string    Base directory for run workspaces (default: current directory at agent startup; env: UNIFIED_AGENT_WORKSPACE_DIR)
```

```markdown
| `UNIFIED_AGENT_WORKSPACE_DIR` | Base directory for run workspaces (default: current directory at agent startup) |
```

Immediately after the agent environment-variable table, add:

```markdown
When no workspace directory is set by flag, environment variable, or config
file, the standard agent uses its startup working directory as an absolute
workspace base. It creates `.ucd-tools`, `working<N>`, and, when needed,
`detached` beneath that directory; it does not remove the base directory or
unrelated files directly beneath it. Set an explicit dedicated path when those
generated directories should be isolated. To preserve the pre-change location
after an upgrade, set `--workspace-dir ~/workspace` (or the equivalent
environment/config value). Existing workspace contents are not migrated.
```

- [ ] **Step 2: Update the agent lifecycle and shim layout documentation**

In `docs/agents.md`, replace the incorrect sibling-tools example with:

```markdown
1. **At startup**, before serving any claims, `cmd/unified-cd-agent`'s `main()`
   writes the `ucd-sh` binary embedded in the agent's own binary
   (`internal/shim/embedded`) to
   `<workspace-dir>/.ucd-tools/ucd-sh` (mode `0755`). Keeping `.ucd-tools`
   inside the workspace base makes it visible to remote container runtimes
   that share that base.
```

Change the workspace-lifecycle opening to:

```markdown
Each concurrency slot owns one slot directory:
`<workspace-dir>/working<N>`. When no override is supplied, `workspace-dir`
is the agent's startup working directory; override it with `--workspace-dir`,
`UNIFIED_AGENT_WORKSPACE_DIR`, or the `workspaceDir` config key.
```

Add one sentence after the layout explanation:

```markdown
The default also places `.ucd-tools` and, for detached runs, `detached`
directly beneath the startup directory; cleanup never removes that base
directory or unrelated direct children.
```

- [ ] **Step 3: Remove the resolved backlog entry**

In `TODO.md`, delete the complete section beginning with:

```markdown
### 9b.
```

and ending immediately before:

```markdown
### 9c.
```

Do not renumber the remaining historical backlog headings.

- [ ] **Step 4: Scan examples, templates, and all live references**

Run:

```powershell
rg -n "workspace-dir|workspaceDir|~/workspace" examples templates
rg -n "default.*~/workspace|same \"~/workspace\" default|wsBase = \"~/workspace\"" cmd internal docs TODO.md examples templates --glob '!docs/superpowers/**'
```

Expected:

- Explicit example/template `workspaceDir` values may remain.
- The second command returns no live statement or code fallback using the old
  default.
- If the first command finds prose that calls `~/workspace` the default,
  update that exact prose to say `current directory at agent startup`.

- [ ] **Step 5: Check documentation formatting and the complete diff**

Run:

```powershell
git diff --check
git diff -- docs/configuration.md docs/agents.md TODO.md examples templates
```

Expected: no whitespace errors, no accidental example changes, and no
statement that existing workspaces are migrated.

- [ ] **Step 6: Commit the documentation**

```powershell
git add docs/configuration.md docs/agents.md TODO.md examples templates
git commit -m "docs: explain current-directory workspace default"
```

- [ ] **Step 7: Run final verification**

Run:

```powershell
go test ./internal/agent -run 'TestResolveWorkspaceDir|TestInstallShim_|TestGCWorkspaces|TestPrepareWorkspace' -count=1
go test ./cmd/unified-cd-agent -count=1
go build -buildvcs=false ./...
go test -buildvcs=false ./...
git diff --check main...HEAD
git status --short --branch
```

Expected:

- Focused tests and both build commands pass.
- CI's Linux full suite passes.
- If the local Windows full suite repeats only the four recorded baseline
  environment failures, record that exact comparison rather than attributing
  it to this change.
- The branch contains the design, implementation, and documentation commits,
  and the worktree is clean.
