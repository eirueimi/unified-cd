# Windows native-step backslash corruption fix — Design

Date: 2026-07-24
Scope: `unified-cd` (agent)

## Background and problem

Running a `native: true` job step on a Windows agent **halves runs of
backslashes** in the script, so the shell misbehaves.

`RunStep` / `RunStepCapture` in `internal/agent/runner.go` pass the step script
to bash as an argv:

```go
cmd := exec.Command(findShell(), "-lc", script)
```

On Windows `findShell()` returns Git Bash (MSYS2). Go's `os/exec` builds the
command line by escaping each argv element with MSVCRT rules
(`syscall.EscapeArg`), while MSYS2's bash parses the command line with its own
rules. This **mismatch** halves runs of backslashes.

Measured (`exec.Command(bash, "-lc", script)`):

| sent | bash received |
|---|---|
| `s\|\\\\\|\\\|g` (`\` x4, x2) | `s\|\\\|\\|g` (x2, x1) |

`s|\\\\|\\|g` (four then two backslashes) becomes `s|\\|\|g` (two then one), and
the trailing `\|` escapes sed's delimiter, so it fails with "unterminated `s'
command".

Additional measurement revealed an important property:

- **Only runs (two or more) of backslashes are halved.**
- **Single backslashes** (`\r`, `\n`, `\"`, a `\` before a letter) **survive
  intact.**
- Bytes passed as an **environment-variable value are not corrupted** (the
  environment block is not subject to argv escaping).

This surfaced on 2026-07-24 when the Unity detection template
(`resolve-unity-path`)'s `sed 's|\\\\|\\|g'` failed on a Windows agent. The
template was rewritten to avoid backslashes as a workaround, but this is a root
bug in the agent that affects **any backslash-containing native script run on a
Windows agent**.

## Goals

- On a Windows agent, a native step's runs of backslashes reach bash intact and
  the script runs as written.
- Non-Windows behavior (Linux/macOS) stays completely unchanged.
- Do not change cancel / process-tree-kill / exit code / stdout+stderr handling.

## Non-goals

- Fixing `RunStepWithShell` (the path where a step declares a custom interpreter
  via `shell:`, e.g. `shell: [python3, -c]`). A custom interpreter cannot use
  `eval` and would need a per-interpreter loader, which is a separate design; it
  is out of scope here. Its Windows limitation is documented in its doc comment.
- Handling the Windows command-line / environment-block length limit for
  extremely large scripts (tens of KB). The argv approach has the same class of
  limit, and this fix does not make it worse.

## Architecture

On Windows only, `RunStep` / `RunStepCapture` pass the script to bash **via an
environment variable** and use a **fixed, backslash-free loader** as the argv:

```
bash -lc 'eval "$__UCD_STEP_SCRIPT"'
```

The value of `__UCD_STEP_SCRIPT` (the actual script) rides on `cmd.Env`. The
environment block is not subject to argv escaping, so the bytes survive and bash
`eval`s it. The `-l` (login shell) flag is kept as today.

Non-Windows is unchanged: `exec.Command("bash", "-lc", script)`, no behavior
change at all.

Measurement confirms this approach preserves backslashes:

| approach | bash received |
|---|---|
| argv (current) | `s\|\\\|\\|g` (halved) |
| env-var + eval (this approach) | `s\|\\\\\|\\\|g` (preserved) |

## Components

### `buildBashStepCmd` (new helper, DRY)

`RunStep` and `RunStepCapture` should share the same command-building logic, so
it lives in one place.

```go
// buildBashStepCmd builds the *exec.Cmd for running a native step's script with
// bash. On Windows the script travels via the __UCD_STEP_SCRIPT environment
// variable and the argv is a fixed, backslash-free loader: Go's Windows argv
// escaping halves runs of backslashes before MSYS bash re-parses the command
// line, which corrupts any script that spells out backslashes (e.g. a sed
// s|\\...). The environment block is not subject to that escaping, so the bytes
// survive. On every other platform the script is passed directly as the -lc
// argument, unchanged.
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

- `baseEnv` is the caller's already-built `StepEnv(exposeEnv, extraEnv)` result.
  The Windows branch appends `__UCD_STEP_SCRIPT=<script>` last (`os/exec` uses
  the last occurrence of a duplicate key).
- The helper does not set `Stdout` / `Stderr` / `Dir` on the cmd (the caller
  sets those as before). Its sole responsibility is building the argv and env
  correctly.

### `RunStep` change

Current (`runner.go:101-110` area):

```go
cmd := exec.Command(findShell(), "-lc", script)
cmd.Stdout = stdout
cmd.Stderr = stderr
cmd.Env = StepEnv(exposeEnv, extraEnv)
if workDir != "" {
    cmd.Dir = workDir
}
```

After:

```go
cmd := buildBashStepCmd(script, StepEnv(exposeEnv, extraEnv))
cmd.Stdout = stdout
cmd.Stderr = stderr
if workDir != "" {
    cmd.Dir = workDir
}
```

### `RunStepCapture` change

Replace the `runner.go:160` area with the same `buildBashStepCmd` route (keep
the existing logic that sets stdout to `&stdoutBuf`).

### `RunStepWithShell` doc note

No behavior change, but the doc comment states:

> Windows note: the script is passed as a process argument, so a custom
> interpreter script that contains runs of backslashes can be corrupted by
> Windows argv escaping. The default bash path (RunStep) avoids this via an
> environment variable; the explicit-shell path does not yet.

## Data flow

```
backend_host.RunDefault
  ├─ step.Shell == nil → RunStep(script, ...)
  │     → buildBashStepCmd(script, StepEnv(...))  // Windows: env+loader / else: argv
  │     → runTreeKilled(ctx, cmd)                 // unchanged
  └─ step.Shell != nil → RunStepWithShell(...)    // unchanged (out of scope)
```

`RunStepCapture` is called (separately from `backend_host`'s native path) to
capture output (e.g. uses:-scope output capture). It uses the same helper.

## Error handling

No new error paths. Only the cmd's argv and env change; execution, cancel,
process-tree-kill, and exit-code extraction from ExitError for the `*exec.Cmd`
handed to `runTreeKilled` are identical to today. `__UCD_STEP_SCRIPT` is always
set here, so it is never undefined and `eval "$__UCD_STEP_SCRIPT"` never evals an
empty string.

## Test plan (TDD)

### Center: backslash-preservation regression test (meaningful on all OSes)

```go
func TestRunStep_PreservesBackslashRuns(t *testing.T) {
    var out, errb bytes.Buffer
    // sed line98's payload; argv escaping would halve it to s|\\|\|g.
    exit, err := RunStep(t.Context(), `printf '%s' 's|\\\\|\\|g'`, &out, &errb, nil, nil, "")
    require.NoError(t, err)
    require.Equal(t, 0, exit)
    require.Equal(t, `s|\\\\|\\|g`, out.String())
}
```

- **FAILs on Windows with the current code** (halved to `s|\\|\|g`), PASSes after
  the fix.
- **PASSes on Unix both before and after** (no such bug there). So the test is a
  Windows regression guard and a correctness check on Unix (it is meaningful
  cross-platform).
- A `RunStepCapture` version (`TestRunStepCapture_PreservesBackslashRuns`) is
  also added, asserting the returned `stdout` is not halved.

### Invariance of existing tests

The existing `RunStep` / `RunStepCapture` / `RunStepWithShell` tests (stdout
capture, non-zero exit, workDir, context cancel,
`TestRunStep_CredentialsNotInheritedByChild`) must all still PASS after the fix.
The credential-non-inheritance test in particular confirms that appending
`__UCD_STEP_SCRIPT` to the env does not break StepEnv's allowlist/denylist
(`stepenv.go`).

### Internal-var non-interference (optional)

Lightly confirm `__UCD_STEP_SCRIPT` does not collide with an existing env var
(an internal double-underscore name is unlikely to collide). It is visible to
the running step but harmless.

## Affected files

| file | change |
|---|---|
| `internal/agent/runner.go` | add `buildBashStepCmd`, route `RunStep`/`RunStepCapture` through it, add a Windows limitation note to `RunStepWithShell`'s doc |
| `internal/agent/runner_test.go` | add backslash-preservation tests for `RunStep`/`RunStepCapture` |

`backend_host.go` needs no change (`RunStep`'s signature is unchanged).

## Open questions

None.
