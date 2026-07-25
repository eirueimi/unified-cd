# Windows native-step backslash corruption fix — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** So that a native step's runs of backslashes are not corrupted on a Windows agent, change `RunStep`/`RunStepCapture` to run the script via an environment variable.

**Architecture:** On Windows only, pass the script to bash via the `__UCD_STEP_SCRIPT` environment variable instead of as an argv, and run a fixed, backslash-free loader `bash -lc 'eval "$__UCD_STEP_SCRIPT"'`. The environment block is not subject to Go's Windows argv escaping, so the bytes survive. Non-Windows keeps the current direct-argv path with unchanged behavior.

**Tech Stack:** Go (`os/exec`, `runtime.GOOS`), testify (`require`/`assert`).

## Global Constraints

- The fix targets only the `RunStep` and `RunStepCapture` paths. `RunStepWithShell` (custom interpreter) is **not changed** (only a Windows limitation note is added to its doc).
- Non-Windows (`runtime.GOOS != "windows"`) behavior is **completely unchanged**. The branch is Windows-only.
- The env var name is `__UCD_STEP_SCRIPT` (fixed). The loader argv is `eval "$__UCD_STEP_SCRIPT"` (contains no backslashes).
- Keep `-l` (login shell) (same as the current `bash -lc`).
- Do not change cancel / process-tree-kill / exit code / stdout+stderr handling (the same `*exec.Cmd` is handed to `runTreeKilled`).
- The "must not break" test string: `s|\\\\|\\|g` (four then two backslashes). Argv escaping halves it to `s|\\|\|g` (two then one).

---

## File Structure

| file | responsibility |
|---|---|
| `internal/agent/runner.go` | add the `buildBashStepCmd` helper and route `RunStep`/`RunStepCapture` through it; add a Windows limitation note to `RunStepWithShell`'s doc |
| `internal/agent/runner_test.go` | add backslash-preservation tests for `RunStep`/`RunStepCapture` |

`backend_host.go` needs no change because `RunStep`/`RunStepCapture` signatures are unchanged.

All commands below run from the repository root.

---

## Task 1: preserve backslashes by running bash via an environment variable

**Files:**
- Modify: `internal/agent/runner.go` (`RunStep` 101-119 / `RunStepCapture` 158-178 / `RunStepWithShell` doc 121-130, and the new `buildBashStepCmd`)
- Test: `internal/agent/runner_test.go`

**Interfaces:**
- Consumes: existing `findShell() string`, `StepEnv(exposeEnv, extraEnv []string) []string`, `runTreeKilled(ctx, *exec.Cmd) error`.
- Produces: `buildBashStepCmd(script string, baseEnv []string) *exec.Cmd` — returns a `*exec.Cmd` with argv and `cmd.Env` built (`Stdout`/`Stderr`/`Dir` unset). On Windows it appends `__UCD_STEP_SCRIPT=<script>` to the env and uses the loader argv. `RunStep`/`RunStepCapture` signatures are unchanged.

- [ ] **Step 1: Write the backslash-preservation test for RunStep (should fail)**

Append to the end of `internal/agent/runner_test.go`. `bytes`, `require`, and `assert` are already imported.

```go
// TestRunStep_PreservesBackslashRuns guards the Windows argv-escaping bug: a
// native step script that spells out runs of backslashes (e.g. a sed
// s|\\\\|\\|g) must reach bash intact. On Windows, passing the script as an
// exec argv halves backslash runs (s|\\|\|g), corrupting the script; the fix
// routes the script through an environment variable instead. On Unix there is
// no such corruption, so this test also documents the expected behavior there.
func TestRunStep_PreservesBackslashRuns(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// printf %s of a single-quoted literal: bash echoes the argument verbatim.
	exit, err := RunStep(t.Context(), `printf '%s' 's|\\\\|\\|g'`, &stdout, &stderr, nil, nil, "")
	require.NoError(t, err)
	assert.Equal(t, 0, exit)
	assert.Equal(t, `s|\\\\|\\|g`, stdout.String(), "backslash runs must survive (stderr: %s)", stderr.String())
}
```

- [ ] **Step 2: Run the test and confirm it fails (Windows)**

Run: `go test ./internal/agent/ -run TestRunStep_PreservesBackslashRuns -v`

Expected (on a Windows dev host): FAIL. `stdout` becomes `s|\\|\|g` (halved) and `assert.Equal` fails with:
```
Error: Not equal: expected: "s|\\\\|\\|g"  actual: "s|\\|\|g"
```
(On Linux there is no bug so it PASSes; the fix targets Windows, so a FAIL on the dev host is the confirmation.)

- [ ] **Step 3: Add the `buildBashStepCmd` helper**

Insert it **immediately before** the `RunStep` function in `internal/agent/runner.go` (before the `RunStep` doc comment block, before the line `// that directory as the working directory.`). `runtime` is already imported.

```go
// buildBashStepCmd builds the *exec.Cmd for running a native step's script with
// bash. On Windows the script travels via the __UCD_STEP_SCRIPT environment
// variable and the argv is a fixed, backslash-free loader (eval "$__UCD_STEP_SCRIPT"):
// Go's Windows argv escaping halves runs of backslashes before MSYS (Git Bash)
// re-parses the command line, which corrupts any script that spells out
// backslashes (e.g. a sed s|\\...|\\...). The environment block is not subject
// to that escaping, so the bytes survive. On every other platform the script is
// passed directly as the -lc argument, unchanged. baseEnv is the caller's
// already-built StepEnv result; the returned cmd has Env set but leaves
// Stdout/Stderr/Dir to the caller.
func buildBashStepCmd(script string, baseEnv []string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		cmd := exec.Command(findShell(), "-lc", `eval "$__UCD_STEP_SCRIPT"`)
		cmd.Env = append(baseEnv, "__UCD_STEP_SCRIPT="+script)
		return cmd
	}
	cmd := exec.Command(findShell(), "-lc", script)
	cmd.Env = baseEnv
	return cmd
}
```

- [ ] **Step 4: Route `RunStep` through the helper**

Replace the `RunStep` body (lines 102-110) in `internal/agent/runner.go`.

Before:

```go
	cmd := exec.Command(findShell(), "-lc", script)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Always set Env: a nil cmd.Env makes os/exec inherit the agent's whole
	// environment, which is exactly the leak StepEnv exists to prevent.
	cmd.Env = StepEnv(exposeEnv, extraEnv)
	if workDir != "" {
		cmd.Dir = workDir
	}
```

After:

```go
	// Env is set inside buildBashStepCmd (never nil): a nil cmd.Env makes
	// os/exec inherit the agent's whole environment, which is exactly the leak
	// StepEnv exists to prevent.
	cmd := buildBashStepCmd(script, StepEnv(exposeEnv, extraEnv))
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if workDir != "" {
		cmd.Dir = workDir
	}
```

- [ ] **Step 5: Run the test and confirm it passes**

Run: `go test ./internal/agent/ -run TestRunStep_PreservesBackslashRuns -v`

Expected: PASS.

- [ ] **Step 6: Write the backslash-preservation test for `RunStepCapture` (should fail)**

Append to `internal/agent/runner_test.go`.

```go
// TestRunStepCapture_PreservesBackslashRuns mirrors
// TestRunStep_PreservesBackslashRuns for the capture path: the returned stdout
// string must contain the un-halved backslash runs.
func TestRunStepCapture_PreservesBackslashRuns(t *testing.T) {
	var stderr bytes.Buffer
	stdout, exit, err := RunStepCapture(t.Context(), `printf '%s' 's|\\\\|\\|g'`, &stderr, nil, nil, "")
	require.NoError(t, err)
	assert.Equal(t, 0, exit)
	assert.Equal(t, `s|\\\\|\\|g`, stdout, "backslash runs must survive (stderr: %s)", stderr.String())
}
```

- [ ] **Step 7: Run the test and confirm it fails**

Run: `go test ./internal/agent/ -run TestRunStepCapture_PreservesBackslashRuns -v`

Expected (Windows dev host): FAIL. `stdout` is halved to `s|\\|\|g` and `assert.Equal` fails.

- [ ] **Step 8: Route `RunStepCapture` through the helper**

Replace the `RunStepCapture` body (lines 160-165) in `internal/agent/runner.go`.

Before:

```go
	var stdoutBuf bytes.Buffer
	cmd := exec.Command(findShell(), "-lc", script)
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = stderr
	// Always set Env: a nil cmd.Env makes os/exec inherit the agent's whole
	// environment, which is exactly the leak StepEnv exists to prevent.
	cmd.Env = StepEnv(exposeEnv, extraEnv)
	if workDir != "" {
		cmd.Dir = workDir
	}
```

After:

```go
	var stdoutBuf bytes.Buffer
	// Env is set inside buildBashStepCmd (never nil): a nil cmd.Env makes
	// os/exec inherit the agent's whole environment, which is exactly the leak
	// StepEnv exists to prevent.
	cmd := buildBashStepCmd(script, StepEnv(exposeEnv, extraEnv))
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = stderr
	if workDir != "" {
		cmd.Dir = workDir
	}
```

- [ ] **Step 9: Run the test and confirm it passes**

Run: `go test ./internal/agent/ -run TestRunStepCapture_PreservesBackslashRuns -v`

Expected: PASS.

- [ ] **Step 10: Add the Windows limitation note to `RunStepWithShell`'s doc**

Append to the end of the `RunStepWithShell` doc comment (lines 121-130) in `internal/agent/runner.go`, right after the `// RunStep's doc comment.` line.

After (the end of that comment becomes):

```go
// (see runTreeKilled). exposeEnv is the agent's ExposeEnv allowlist; see
// RunStep's doc comment.
//
// Windows note: the script is passed as a process argument, so a custom
// interpreter script that contains runs of backslashes can be corrupted by
// Windows argv escaping (Go escapes with MSVCRT rules, MSYS bash parses with
// its own). The default bash path (RunStep) avoids this via the
// __UCD_STEP_SCRIPT environment variable; this explicit-shell path does not yet.
```

- [ ] **Step 11: Run the whole agent test suite and confirm no regressions**

Run: `go test ./internal/agent/ 2>&1 | tail -20`

Expected: `ok  	github.com/eirueimi/unified-cd/internal/agent`. All existing tests pass, including `TestRunStep_CapturesStdout`, `TestRunStep_NonZeroExit`, `TestRunStep_WorkDir`, `TestRunStep_RespectsContextCancel`, `TestRunStepCapture_ReturnsStdout`, `TestRunStepCapture_WorkDir`, `TestRunStep_CredentialsNotInheritedByChild`, and the `TestRunStepWithShell_*` group.

> Some `internal/agent` tests may use Docker (see the CI-flake memory). The runner unit tests do not need Docker. If an unrelated Docker-dependent test fails for environmental reasons, narrow to `-run 'RunStep|RunStepCapture|RunStepWithShell|StepEnv'` to confirm this task's scope is green.

- [ ] **Step 12: Commit**

```bash
git add internal/agent/runner.go internal/agent/runner_test.go
git commit -F - <<'EOF'
fix(agent): preserve backslash runs in Windows native step scripts

On Windows, exec.Command(bash, "-lc", script) has Go escape the script argv with
MSVCRT rules while MSYS Git Bash parses the command line with its own rules,
halving runs of backslashes: s|\\\\|\\|g arrives as s|\\|\|g, which makes sed
fail with "unterminated s command". Route the script through the __UCD_STEP_SCRIPT
environment variable on Windows and run a fixed, backslash-free loader
(eval "$__UCD_STEP_SCRIPT"); the environment block is not subject to argv
escaping, so the bytes survive. Non-Windows keeps the direct argv path unchanged.

RunStepWithShell (custom interpreters, e.g. shell: [python3, -c]) is out of scope;
its Windows limitation is documented in its doc comment.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
```

---

## Self-Review

**1. Spec coverage:**
- "Windows-only env-var+eval" -> Task 1 Steps 3-4, 8 (the Windows branch of `buildBashStepCmd`).
- "Non-Windows unchanged" -> the else branch in Step 3 (direct argv).
- "`buildBashStepCmd` DRY helper" -> Step 3, used by both paths in Steps 4/8.
- "Backslash-preservation tests (RunStep/RunStepCapture)" -> Steps 1, 6.
- "Existing tests stay green" -> Step 11.
- "`RunStepWithShell` unchanged, doc note only" -> Step 10.
- "cancel/tree-kill/exit code unchanged" -> `runTreeKilled` and the ExitError handling are untouched (Steps 4/8 only swap the cmd construction).
- Every spec requirement maps to a task. No gaps.

**2. Placeholder scan:** No TBD/TODO/vague instructions. Every code step contains complete code.

**3. Type consistency:** `buildBashStepCmd(script string, baseEnv []string) *exec.Cmd` matches between its Step 3 definition and its Step 4/8 call sites. The env var name `__UCD_STEP_SCRIPT` matches across the loader argv, the helper, the spec, the doc, and the commit. The test string `s|\\\\|\\|g` matches across all steps.
