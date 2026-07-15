# Path Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix native cache path resolution (G1), inject `UNIFIED_WORKSPACE` per mode (G4), reject artifact/cache paths that escape the workspace (F-PATH-1 Critical + F-PATH-3), harden `sanitizeJobName` (F-PATH-2), and stop git-ref argument injection.

**Architecture:** The artifact/cache resolve seam (`ExecBackend.ResolveArtifactPath`/`ResolveCachePath`) gains an error return and a shared containment guard, so absolute and `..`-escaping paths fail the step instead of silently escaping — this fixes G1 (native cache now resolves against the workspace like every other mode) and closes the k8s sidecar traversal. A new `WorkspacePath` backend method feeds `UNIFIED_WORKSPACE`. Two small standalone fixes cover the job-name sanitizer and git-ref validation.

**Tech Stack:** Go, testify, `store.NewTestPostgres` (dockerized Postgres for integration tests).

**Spec:** `docs/superpowers/specs/2026-07-15-path-hardening-design.md`

## Global Constraints

- All code, comments, commit messages, docs in **English** (AGENTS.md). No PII.
- Worktree `../unified-cd-path-hardening`, branch `path-hardening` (base main). Never commit from the main tree.
- Containment error message (grep-able): `artifact/cache path %q escapes the workspace`.
- Relative in-bounds paths are unaffected; only absolute and `..`-escaping paths change behavior (now a step failure).
- `UNIFIED_WORKSPACE` = the step's cwd workspace root: native → `workDir`, isolated → host mount path, k8s → pod mount path, scope (both) → `/workspace`. User `env:` overrides it (same precedence as `UNIFIED_AGENT_OS`).
- git ref allowlist: `^[A-Za-z0-9][A-Za-z0-9._/+-]*$` (must not start with `-`).
- Store/integration tests need Docker (skip under `-short`). `docs/field-reference.md` is generated — untouched.

---

### Task 1: Path containment helpers

**Files:**
- Create: `internal/agent/pathguard.go`
- Test: `internal/agent/pathguard_test.go` (create)

**Interfaces:**
- Produces (Task 2 consumes):
  - `containWithinOS(root, p string) (string, error)` — OS-native (`path/filepath`) join+containment for host paths.
  - `containWithinSlash(root, p string) (string, error)` — forward-slash (`path`) join+containment for container paths.
  - Both: empty `p` → `(root, nil)`; absolute `p` → error; a cleaned result outside `root` → error (message `artifact/cache path %q escapes the workspace`); otherwise the cleaned joined path.

- [ ] **Step 1: Write the failing tests**

Create `internal/agent/pathguard_test.go`:

```go
package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainWithinSlash(t *testing.T) {
	root := "/workspace"
	ok := map[string]string{
		"":              "/workspace",
		"node_modules":  "/workspace/node_modules",
		"a/b/c":         "/workspace/a/b/c",
		"foo/../bar":    "/workspace/bar", // stays in bounds after cleaning
		"./dist":        "/workspace/dist",
	}
	for in, want := range ok {
		got, err := containWithinSlash(root, in)
		require.NoError(t, err, "input %q", in)
		assert.Equal(t, want, got, "input %q", in)
	}
	bad := []string{"../etc/passwd", "..", "a/../../b", "/etc/passwd", "/workspace/../x"}
	for _, in := range bad {
		_, err := containWithinSlash(root, in)
		require.Error(t, err, "input %q must be rejected", in)
		assert.Contains(t, err.Error(), "escapes the workspace", "input %q", in)
	}
}

func TestContainWithinOS(t *testing.T) {
	// Use a slash root; on Windows filepath still treats it as relative-to-drive
	// but the containment logic (prefix of cleaned join) holds for the in-bounds
	// cases and rejects the traversal cases regardless of separator.
	root := "/tmp/ws"
	got, err := containWithinOS(root, "node_modules")
	require.NoError(t, err)
	assert.Equal(t, filepathJoin(root, "node_modules"), got)

	_, err = containWithinOS(root, "../../etc/passwd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes the workspace")

	// Absolute is rejected.
	_, err = containWithinOS(root, absForTest())
	require.Error(t, err)
}
```

Add two tiny test helpers at the bottom of the file so the assertions are OS-portable:

```go
func filepathJoin(a, b string) string { return filepath.Join(a, b) }
func absForTest() string {
	if runtime.GOOS == "windows" {
		return `C:\Windows\system32`
	}
	return "/etc/passwd"
}
```

and import `"path/filepath"` and `"runtime"` in the test file.

- [ ] **Step 2: Run to verify failure**

Run: `cd /path/to/unified-cd-path-hardening && go test ./internal/agent/ -run 'TestContainWithin' -v`
Expected: compile errors (`undefined: containWithinOS`, `containWithinSlash`).

- [ ] **Step 3: Implement**

Create `internal/agent/pathguard.go`:

```go
package agent

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// containWithinSlash joins a RELATIVE forward-slash path p under root (a
// container path, always Linux) and guarantees the cleaned result stays
// within root. An empty p is the root itself. An absolute p, or any p that
// escapes root via "..", is rejected — this is the containment that stops a
// crafted artifact/cache path from reaching files outside the workspace
// (e.g. the artifact sidecar's mounted secrets on k8s).
func containWithinSlash(root, p string) (string, error) {
	if p == "" {
		return root, nil
	}
	if path.IsAbs(p) {
		return "", fmt.Errorf("artifact/cache path %q escapes the workspace", p)
	}
	joined := path.Clean(path.Join(root, p))
	if joined != root && !strings.HasPrefix(joined, root+"/") {
		return "", fmt.Errorf("artifact/cache path %q escapes the workspace", p)
	}
	return joined, nil
}

// containWithinOS is containWithinSlash for host (OS-native) paths.
func containWithinOS(root, p string) (string, error) {
	if p == "" {
		return root, nil
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("artifact/cache path %q escapes the workspace", p)
	}
	cleanRoot := filepath.Clean(root)
	joined := filepath.Clean(filepath.Join(cleanRoot, p))
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("artifact/cache path %q escapes the workspace", p)
	}
	return joined, nil
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/agent/ -run 'TestContainWithin' -v && go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/pathguard.go internal/agent/pathguard_test.go
git commit -m "feat(agent): path containment helpers rejecting workspace escape"
```

---

### Task 2: Resolvers return (string, error) with containment — G1 + F-PATH-1

**Files:**
- Modify: `internal/agent/backend.go:58,68` (interface), `internal/agent/agent.go:397,413` (`resolveWorkspacePath`, `resolveScopePath`), `internal/agent/backend_host.go:276-301` (`ResolveArtifactPath`, `ResolveCachePath`), `internal/k8sagent/backend.go:296-308`, `internal/agent/orchestrator.go:699,728,809` (callers)
- Test: `internal/agent/backend_host_test.go` (extend or create) + `internal/k8sagent/` resolver test

**Interfaces:**
- Consumes: `containWithinOS`, `containWithinSlash` (Task 1).
- Produces: `ExecBackend.ResolveArtifactPath(scope ScopeHandle, p string) (string, error)` and `ResolveCachePath(...) (string, error)`; host and k8s both implement them with containment; host cache is now identical to host artifact (**G1 fix**); orchestrator's three callers propagate the error as a step failure.

- [ ] **Step 1: Write the failing tests**

Add to a host backend test file (`internal/agent/backend_host_test.go` — create if absent) a resolver containment + G1 test:

```go
package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostResolve_ContainmentAndG1(t *testing.T) {
	// native backend: pod == nil
	b := &hostBackend{workDir: "/tmp/ws"}

	// G1: a non-scoped native CACHE path now resolves against workDir
	// (previously returned unresolved).
	got, err := b.ResolveCachePath(ScopeHandle{}, "node_modules")
	require.NoError(t, err)
	assert.Equal(t, filepathJoin("/tmp/ws", "node_modules"), got)

	// artifact path resolves the same way
	got, err = b.ResolveArtifactPath(ScopeHandle{}, "dist")
	require.NoError(t, err)
	assert.Equal(t, filepathJoin("/tmp/ws", "dist"), got)

	// containment: traversal rejected for both
	_, err = b.ResolveArtifactPath(ScopeHandle{}, "../../etc/passwd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes the workspace")
	_, err = b.ResolveCachePath(ScopeHandle{}, "../../etc/passwd")
	require.Error(t, err)
}
```

(Reuse the `filepathJoin` helper from Task 1's test file — same package.) For k8s, add to a backend test in `internal/k8sagent/`:

```go
func TestK8sResolve_Containment(t *testing.T) {
	b := &k8sBackend{mountPath: "/workspace"}
	got, err := b.ResolveArtifactPath(agentlib.ScopeHandle{}, "reports")
	require.NoError(t, err)
	assert.Equal(t, "/workspace/reports", got)

	_, err = b.ResolveArtifactPath(agentlib.ScopeHandle{}, "../../proc/self/environ")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes the workspace")
	_, err = b.ResolveCachePath(agentlib.ScopeHandle{}, "/etc/passwd")
	require.Error(t, err)
}
```

(Check the k8s test package's existing imports for the `agentlib` alias and `k8sBackend` field names; adjust `mountPath` to the real field — grep `mountPath` in `internal/k8sagent/backend.go`.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agent/ ./internal/k8sagent/ -run 'Resolve' -v`
Expected: compile errors (signatures still return only `string`; the tests expect `(string, error)`).

- [ ] **Step 3: Implement — interface, resolvers, callers**

`internal/agent/backend.go` — change the two interface lines to return `(string, error)` and update their doc comments to state that an absolute or escaping path is now an error (drop the "returned unchanged" clause).

`internal/agent/agent.go` — replace the two helpers:

```go
func resolveWorkspacePath(workDir, p string) (string, error) {
	return containWithinOS(workDir, p)
}

func resolveScopePath(p string) (string, error) {
	return containWithinSlash(scopeWorkDir, p)
}
```

(Keep/trim the surrounding doc comments; the "already-absolute returned unchanged" sentences are now false — replace with "an absolute or escaping path is rejected".)

`internal/agent/backend_host.go` — both methods return `(string, error)`; **cache becomes identical to artifact** (delete the `return p` native branch, delete the `b.pod != nil` special-case — all non-scoped host paths resolve against `workDir`):

```go
func (b *hostBackend) ResolveArtifactPath(scope ScopeHandle, p string) (string, error) {
	if !scope.IsZero() {
		return resolveScopePath(p)
	}
	return resolveWorkspacePath(b.workDir, p)
}

// ResolveCachePath is identical to ResolveArtifactPath: a non-scoped cache
// path resolves against the claim workspace in every mode (native included).
// The pre-fix native branch left it unresolved, which tarred the agent
// process CWD instead of the workspace (G1).
func (b *hostBackend) ResolveCachePath(scope ScopeHandle, p string) (string, error) {
	return b.ResolveArtifactPath(scope, p)
}
```

`internal/k8sagent/backend.go`:

```go
func (b *k8sBackend) ResolveArtifactPath(scope agentlib.ScopeHandle, p string) (string, error) {
	if !scope.IsZero() {
		return containWithinSlash(scopeMountPath, p)
	}
	return containWithinSlash(b.mountPath, p)
}

func (b *k8sBackend) ResolveCachePath(scope agentlib.ScopeHandle, p string) (string, error) {
	return b.ResolveArtifactPath(scope, p)
}
```

`containWithinSlash` lives in package `agent`; k8s imports it as `agentlib.` — if it is unexported there, **export it** as `agentlib.ContainWithinSlash` (rename in Task 1's file and the host callers) so k8s can call it. Prefer exporting both helpers (`ContainWithinOS`, `ContainWithinSlash`) from the start if the k8s package can't see unexported agent symbols — adjust Task 1 accordingly and keep host call sites using the exported names.

`internal/agent/orchestrator.go` — the three call sites now handle the error, failing the step:

- `executeUploadArtifact` (~:699):
```go
	artifactPath, err := b.ResolveArtifactPath(scope, ua.Path)
	if err != nil {
		slog.Error("upload-artifact path rejected", "step", step.Name, "error", err)
		_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
			RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Failed",
			StartedAt: started, EndedAt: time.Now().UTC(),
		})
		return fmt.Errorf("upload-artifact %q: %w", ua.Name, err)
	}
```
- `executeDownloadArtifact` (~:728): same shape around `resolvedDestDir, err := b.ResolveArtifactPath(scope, destDir)`.
- `executeCacheStep` (~:809): `scopedCachePath, err := b.ResolveCachePath(scope, cachePath)` — on error, log and return it (cache steps currently warn+skip on failure; here a path-escape is a hard step error, so return the error rather than skipping). Match the function's existing error-return convention.

Fix any other caller the compiler flags (`go build ./...`).

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/agent/ ./internal/k8sagent/ -count=1 && go build ./...`
Expected: PASS including pre-existing resolver/orchestrator tests (relative-path callers are unaffected; only the signature changed for them — update any test that called the resolvers expecting a single return value).

- [ ] **Step 5: Commit**

```bash
git add internal/agent/backend.go internal/agent/agent.go internal/agent/backend_host.go internal/agent/backend_host_test.go internal/k8sagent/backend.go internal/agent/orchestrator.go internal/agent/pathguard.go
git add -A internal/k8sagent
git commit -m "fix(agent): resolve artifact/cache paths with containment; fix native cache (G1, F-PATH-1)"
```

---

### Task 3: `UNIFIED_WORKSPACE` injection — G4

**Files:**
- Modify: `internal/agent/backend.go` (interface: add `WorkspacePath`), `internal/agent/backend_host.go` (impl + store mount path), `internal/k8sagent/backend.go` (impl), `internal/agent/orchestrator.go:397` (inject), `internal/k8sagent/agent.go:310` (`imageStepEnv`)
- Test: extend `internal/agent/agent_test.go` (the `UNIFIED_AGENT_OS` integration test) + `internal/k8sagent/agent_env_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `ExecBackend.WorkspacePath(scope ScopeHandle) string`; every non-scoped/scoped step gets `UNIFIED_WORKSPACE` in its env, user-overridable.

- [ ] **Step 1: Write the failing tests**

Extend the existing `UNIFIED_AGENT_OS` exposure test (`internal/agent/agent_test.go`, ~:451) with a parallel `UNIFIED_WORKSPACE` assertion — a `run:` step `test "$UNIFIED_WORKSPACE" = "$PWD"` must succeed (native: cwd is workDir). Add a unit test for the method:

```go
func TestHostWorkspacePath(t *testing.T) {
	native := &hostBackend{workDir: "/tmp/ws"}
	assert.Equal(t, "/tmp/ws", native.WorkspacePath(ScopeHandle{}))
	// scoped is always the container cwd
	assert.Equal(t, scopeWorkDir, native.WorkspacePath(scopeHandleForTest()))
}
```

(For `scopeHandleForTest()` construct a non-zero `ScopeHandle` the way the package's other tests do — grep existing tests for how a scope handle is built; if there's no easy constructor, assert only the native/isolated cases and cover scope via the k8s test.) For k8s, add to `agent_env_test.go`: a pod-exec step's env contains `UNIFIED_WORKSPACE=<mountPath>` (mirror the existing `UNIFIED_AGENT_OS=linux` assertion).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agent/ -run 'WorkspacePath|ExposesAgentOS' -v`
Expected: compile error (`WorkspacePath` undefined) / the `$UNIFIED_WORKSPACE` step fails (var unset).

- [ ] **Step 3: Implement**

`internal/agent/backend.go` — add to the interface:

```go
	// WorkspacePath returns the cwd workspace root a step sees in this scope
	// (host workDir natively; the container mount path in isolated/k8s; the
	// scope container's cwd when scoped), exposed to steps as UNIFIED_WORKSPACE.
	WorkspacePath(scope ScopeHandle) string
```

`internal/agent/backend_host.go` — store the isolated mount path on the struct so the method can return it. Add a field `mountPath string` to `hostBackend` and set it where the backend is constructed (the same place `pod` is wired, using `hostNamedMountPath(c.PodTemplate)` — grep the `hostBackend{` literal). Then:

```go
func (b *hostBackend) WorkspacePath(scope ScopeHandle) string {
	if !scope.IsZero() {
		return scopeWorkDir
	}
	if b.pod != nil {
		return b.mountPath // isolated: the in-container bind-mount path
	}
	return b.workDir // native: the host working directory (also the step cwd)
}
```

`internal/k8sagent/backend.go`:

```go
func (b *k8sBackend) WorkspacePath(scope agentlib.ScopeHandle) string {
	if !scope.IsZero() {
		return scopeMountPath
	}
	return b.mountPath
}
```

`internal/agent/orchestrator.go` (~:397) — add the var to `extraEnv` before the user `env:` loop (so user overrides win):

```go
	extraEnv := []string{
		"UNIFIED_AGENT_OS=" + agentOSForStep(step, b.DefaultAgentOS()),
		"UNIFIED_WORKSPACE=" + b.WorkspacePath(scope),
	}
```

`internal/k8sagent/agent.go` `imageStepEnv` (~:310) — set `env["UNIFIED_WORKSPACE"] = scopeMountPath` (this map builds the env for scoped `runsIn.image` steps, which run in the scope pod at `/workspace`), keeping the existing "always a new map, never mutate the claim" contract.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/agent/ ./internal/k8sagent/ -count=1 && go build ./...`
Expected: PASS. The parity suites (`internal/paritycases`, `internal/k8sagent/parity_k8s_test.go`) must stay green — if a parity test snapshots the exact env set, add `UNIFIED_WORKSPACE` to its expected set (do not drop the assertion).

- [ ] **Step 5: Commit**

```bash
git add internal/agent/backend.go internal/agent/backend_host.go internal/agent/agent.go internal/agent/agent_test.go internal/k8sagent/backend.go internal/k8sagent/agent.go internal/k8sagent/agent_env_test.go
git commit -m "feat(agent): expose UNIFIED_WORKSPACE to steps (G4)"
```

---

### Task 4: `sanitizeJobName` hardening — F-PATH-2

**Files:**
- Modify: `internal/agent/workspace.go:23-37`
- Test: `internal/agent/workspace_test.go` (extend or create)

**Interfaces:**
- Produces: `sanitizeJobName` never returns `.`, `..`, an all-dots string, or a Windows reserved device name.

- [ ] **Step 1: Write the failing test**

```go
func TestSanitizeJobName_Degenerate(t *testing.T) {
	for _, in := range []string{"..", ".", "...", "con", "CON", "Prn", "nul", "aux", "com1", "COM9", "lpt1", "nul.txt"} {
		got := sanitizeJobName(in)
		assert.NotContains(t, []string{".", "..", "..."}, got, "input %q", in)
		assert.NotEqual(t, "con", strings.ToLower(got), "input %q -> reserved", in)
		require.NotEmpty(t, got)
		// The result must be a safe single segment (no dot-only, not reserved).
	}
	// ordinary names pass through unchanged
	assert.Equal(t, "my-job", sanitizeJobName("my-job"))
	assert.Equal(t, "job_1.2", sanitizeJobName("job_1.2"))
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agent/ -run TestSanitizeJobName_Degenerate -v`
Expected: FAIL (`sanitizeJobName("..")` returns `".."`, `sanitizeJobName("con")` returns `"con"`).

- [ ] **Step 3: Implement**

Replace the tail of `sanitizeJobName` (after the rune-filter builds `b`):

```go
	out := b.String()
	if out == "" {
		return "job"
	}
	// Reject degenerate results that are unsafe as a path segment: dot-only
	// names (".", "..", "...") escape or self-reference, and Windows reserved
	// device names break on a Windows agent. Upstream DSL validation already
	// blocks these, but this function is the defensive last line.
	if strings.Trim(out, ".") == "" {
		return "job"
	}
	if isWindowsReservedName(out) {
		return "job_" + out
	}
	return out
}

// isWindowsReservedName reports whether name (case-insensitive, ignoring any
// extension) is a Windows reserved device name.
func isWindowsReservedName(name string) bool {
	base := strings.ToUpper(name)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	switch base {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}
	return false
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/agent/ -run TestSanitizeJobName -v && go build ./...`
Expected: PASS (new + any pre-existing sanitize tests).

- [ ] **Step 5: Commit**

```bash
git add internal/agent/workspace.go internal/agent/workspace_test.go
git commit -m "fix(agent): sanitizeJobName rejects dot-only and Windows reserved names"
```

---

### Task 5: git-ref injection

**Files:**
- Modify: `internal/gittemplate/uri.go:20-55` (`ParseURI` ref allowlist), `internal/gittemplate/fetch.go:79,134,230,285` (`--` before the ref)
- Test: `internal/gittemplate/uri_test.go` (extend)

**Interfaces:**
- Produces: `ParseURI` rejects a ref that starts with `-` or contains characters outside `[A-Za-z0-9._/+-]`; `git fetch`/`git ls-remote` receive `--` before the refspec.

- [ ] **Step 1: Write the failing test**

Add to `internal/gittemplate/uri_test.go`:

```go
func TestParseURI_RefAllowlist(t *testing.T) {
	ok := []string{
		"git://h/o/r/p@main",
		"git://h/o/r/p@v1.2.3",
		"git://h/o/r/p@feature/x",
		"git://h/o/r/p@a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0",
	}
	for _, u := range ok {
		_, err := ParseURI(u)
		require.NoError(t, err, u)
	}
	bad := []string{
		"git://h/o/r/p@-x",
		"git://h/o/r/p@--upload-pack=y",
		"git://h/o/r/p@ref with space",
		"git://h/o/r/p@ref;rm -rf",
	}
	for _, u := range bad {
		_, err := ParseURI(u)
		require.Error(t, err, u)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/gittemplate/ -run TestParseURI_RefAllowlist -v`
Expected: FAIL (the `@-x` / `@--upload-pack=y` cases currently parse successfully).

- [ ] **Step 3: Implement**

`internal/gittemplate/uri.go` — add a package-level regexp and a check after the empty-ref guard:

```go
var refAllowed = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+-]*$`)
```

then, replacing the `if ref == ""` block's success fall-through:

```go
	if ref == "" {
		return URI{}, fmt.Errorf("git URI has empty ref in %q", raw)
	}
	if !refAllowed.MatchString(ref) {
		return URI{}, fmt.Errorf("git URI ref %q contains invalid characters (must match %s)", ref, refAllowed.String())
	}
```

Add `"regexp"` to the imports.

`internal/gittemplate/fetch.go` — at all four sites, insert `"--"` before the ref so git can never parse it as an option (belt-and-braces with the allowlist):

- `:79` → `run("fetch", "--depth=1", repoURL, "--", ref)`
- `:134` → `run("fetch", "--depth=1", repoURL, "--", uri.Ref)`
- `:230` → `exec.CommandContext(ctx, "git", "ls-remote", repoURL, "--", ref)`
- `:285` → `runGit("fetch", "--depth=1", repoURL, "--", ref)`

(Confirm each `run`/`runGit`/`exec.CommandContext` builds argv as a slice — inserting `"--"` as a separate element is correct; do NOT concatenate it into the URL.)

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/gittemplate/ -count=1 && go build ./...`
Expected: PASS including pre-existing gittemplate tests (a test asserting exact `git` argv must gain the `--` element — update its expectation, do not remove the assertion).

- [ ] **Step 5: Commit**

```bash
git add internal/gittemplate/uri.go internal/gittemplate/fetch.go internal/gittemplate/uri_test.go
git commit -m "fix(gittemplate): validate git ref and separate it with -- to block option injection"
```

---

### Task 6: Documentation + final sweep

**Files:**
- Modify: `docs/jobs.md` (artifact/cache path rules + `UNIFIED_WORKSPACE`), `docs/troubleshooting.md` (two entries)
- Modify: `docs/configuration.md` OR wherever `UNIFIED_AGENT_OS` is documented (add `UNIFIED_WORKSPACE`)

**Interfaces:**
- Consumes: the exact strings from Tasks 2 and 5.

- [ ] **Step 1: `docs/jobs.md`**

In the artifact/cache step reference, add:

> The `path` of an `uploadArtifact`/`downloadArtifact`/`cache` step must be **relative** to the run workspace. Relative paths behave identically across native, isolated, and Kubernetes execution. Absolute paths and paths that escape the workspace (via `..`) are rejected — the step fails with `artifact/cache path ... escapes the workspace`. Inside a step, `$UNIFIED_WORKSPACE` names the current workspace root (the step's working directory), so scripts can build workspace-relative paths portably.

- [ ] **Step 2: `docs/troubleshooting.md`** (mirror the Symptom/Cause/Fix format)

Entry 1:
> **Symptom:** a step fails with `artifact/cache path "<p>" escapes the workspace`.
> **Cause:** an `uploadArtifact`/`downloadArtifact`/`cache` step used an absolute path or a `..` path that leaves the run workspace. This is rejected to keep steps from reading or writing files outside the workspace (on Kubernetes the artifact sidecar is more privileged than the job container).
> **Fix:** use a path relative to the workspace (e.g. `dist`, not `/workspace/dist` or `../dist`). `$UNIFIED_WORKSPACE` names the workspace root if you need an absolute base.

Entry 2:
> **Symptom:** a `uses: git://...` job fails to resolve with `git URI ref "..." contains invalid characters`.
> **Cause:** the `@ref` portion contains characters outside `[A-Za-z0-9._/+-]` or starts with `-` (blocked to prevent git option injection).
> **Fix:** reference a normal branch, tag, or SHA.

- [ ] **Step 3: `UNIFIED_WORKSPACE` doc**

Wherever `UNIFIED_AGENT_OS` is documented (grep `UNIFIED_AGENT_OS` under `docs/`), add a sibling line for `UNIFIED_WORKSPACE` — "the absolute path of the run workspace as seen inside the step (the step's working directory); user `env:` may override it."

- [ ] **Step 4: Sweep and full test run**

```bash
grep -rn "UNIFIED_WORKSPACE\|escapes the workspace\|contains invalid characters" docs/ internal/ | grep -v _test.go
go build ./... && go test ./internal/... -count=1
git status   # only the doc files in this task's diff
```

Expected: strings consistent between code and docs; tests PASS.

- [ ] **Step 5: Commit**

```bash
git add docs/jobs.md docs/troubleshooting.md docs/configuration.md
git commit -m "docs: workspace-relative path rules, UNIFIED_WORKSPACE, git-ref troubleshooting"
```
