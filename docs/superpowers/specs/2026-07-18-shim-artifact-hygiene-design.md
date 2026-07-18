# Shim Artifact Hygiene Design

## Goal

Keep generated `ucd-sh-*` binaries out of the Git worktree while preserving
reliable builds, tests, and development containers.

## Problem

The agent development Air command writes a generated shim directly into the
bind-mounted repository. The placeholder is tracked, so `docker compose up -d`
leaves a binary diff in the worktree. A fresh clone also needs a file for each
architecture because `go:embed` rejects a missing path before tests can run.

## Design

1. Stop tracking `internal/shim/embedded/ucd-sh-amd64` and
   `internal/shim/embedded/ucd-sh-arm64`. Keep the existing `ucd-sh-*`
   `.gitignore` rule so generated binaries remain ignored.
2. Add a deterministic preparation command that creates empty placeholders
   for both architectures when they do not exist. It must never overwrite a
   non-empty generated shim.
3. Make the repository test targets and CI invoke the preparation command
   before Go compilation. Direct `go test` remains a lower-level command; the
   supported test entrypoint prepares placeholders first.
4. Change the Air agent build command to copy the source to a container-local
   temporary build directory, generate the shim there, and compile the agent
   from that copy. Air continues watching the bind-mounted source, but no
   generated binary is written to it.
5. Production Docker and release builders continue generating real shims in
   their isolated build contexts. No real shim bytes are committed.

## Verification

- A clean checkout can run the supported test target and CI without tracked
  shim files.
- `docker compose up -d` does not change `git status` after the agent rebuilds.
- A production agent build contains a non-empty embedded shim.
- The CI guard verifies that shim binaries are absent from Git rather than
  requiring tracked zero-byte blobs.

## Non-goals

- This change does not migrate build orchestration from Make to go-task.
  That migration is a separate PR with its own compatibility and developer
  workflow review.
