# Default Agent Workspace to the Current Directory

**Date:** 2026-07-26
**Status:** Approved (brainstorming)

## Motivation

`unified-cd-agent` currently falls back to `~/workspace` when no workspace
directory is supplied. This places agent state in a location unrelated to the
directory from which the operator launched the agent and is especially
surprising for ad hoc Windows agents.

The default should instead be the agent process's startup working directory.
An operator who starts the agent from `C:\agents\unity`, for example, should
get `working0`, `detached`, and `.ucd-tools` beneath
`C:\agents\unity` without having to repeat `--workspace-dir`.

## User-Visible Behavior

When all three configuration sources are unset:

- the `--workspace-dir` flag,
- `UNIFIED_AGENT_WORKSPACE_DIR`, and
- the `workspaceDir` configuration-file key,

the workspace base is the process working directory captured during agent
startup. The default is resolved to an absolute path.

The existing layout beneath that base is unchanged:

```text
<startup-working-directory>/
  .ucd-tools/
  working0/
  working1/
  detached/
```

Only directories required by the configured concurrency and claimed run types
are created.

Explicit workspace configuration keeps its existing behavior, including `~/`
expansion and support for relative paths. The configuration precedence is also
unchanged.

## Path Resolution

Introduce one workspace-path resolver in `internal/agent` and use it everywhere
an unset `WorkspaceDir` is interpreted:

1. If the value is empty, call `os.Getwd()` and return that absolute startup
   working directory.
2. Otherwise, preserve the current `~/` expansion behavior.
3. Return a contextual error if the current working directory or home directory
   cannot be resolved.

`cmd/unified-cd-agent` resolves the effective workspace directory once before
installing the shell shim. It passes the same resolved value to
`InstallShim` and `Agent.WorkspaceDir`, preventing the startup and run paths
from interpreting the default differently.

`Agent.Run` and `InstallShim` retain defensive resolution for direct package
callers that pass an empty value.

## Safety and Cleanup

Using the startup directory as the workspace base does not broaden cleanup:

- `--clean-workspace` removes only a specific
  `working<slot>/<sanitized-job-name>` directory.
- Detached-run cleanup removes only the corresponding
  `detached/<sanitized-run-id>` directory.
- Workspace retention GC scans job directories beneath `working<slot>` and
  never removes the workspace base, a `working<slot>` directory, or
  `.ucd-tools`.

The agent therefore never deletes the startup working directory itself or
unrelated files placed directly in it. The documentation must nevertheless
make the generated top-level directories visible so operators can choose an
explicit dedicated path when desired.

## Code and Test Changes

- Replace the duplicated `~/workspace` fallbacks in `internal/agent/agent.go`
  with the shared resolver.
- Resolve the effective path once in `cmd/unified-cd-agent/main.go`, before
  `InstallShim`, and assign the resolved path to the agent.
- Update the CLI help text to describe the startup working-directory default.
- Replace the existing empty-workspace `InstallShim` test with a test that
  changes into a temporary directory and verifies
  `<cwd>/.ucd-tools/ucd-sh`.
- Add focused resolver tests for the empty default, explicit paths, and `~/`
  expansion.
- Add or update command-level coverage for the resolved value if the existing
  `cmd/unified-cd-agent` test seams support it without invoking `main`.

## Documentation and Repository Hygiene

Update all live references to the old default, including:

- `docs/configuration.md`,
- `docs/agents.md`,
- CLI help text,
- the stale workspace-default entry in `TODO.md`, and
- any matching examples or templates found by a repository-wide scan.

Examples and templates that explicitly configure `workspaceDir` remain valid;
they should not be rewritten merely because the fallback changed.

Historical material under `docs/superpowers/` remains unchanged except for
this design and its implementation plan.

## Compatibility

This is an intentional default-location change. Existing agents that omit all
workspace configuration will stop using `~/workspace` and start using their
startup directory after upgrading. Operators who require the old location can
preserve it explicitly with:

```text
--workspace-dir ~/workspace
```

or the equivalent environment variable or configuration-file key.

Persisted workspaces are not migrated automatically. This avoids silently
moving or deleting existing job state and keeps rollback behavior predictable.

## Verification

- Focused unit tests for workspace path resolution and shim installation pass.
- `go test ./cmd/unified-cd-agent ./internal/agent` passes in a correctly
  provisioned shell environment.
- `go test ./...` passes in CI.
- A repository-wide scan outside historical `docs/superpowers/` content finds
  no live claim that the default is `~/workspace`.
- Manual help output reports the current-directory default.
