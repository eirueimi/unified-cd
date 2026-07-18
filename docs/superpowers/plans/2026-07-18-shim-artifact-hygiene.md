# Shim Artifact Hygiene Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep generated `ucd-sh-*` binaries ignored while preserving reliable tests, CI, Compose development, and release builds.

**Architecture:** A preparation script creates missing zero-byte embed inputs without truncating real shims. Make and CI invoke it before Go compilation. Air builds real shims only in a container-local source copy.

**Tech Stack:** Go 1.26, Bash, GNU Make, Air, Docker Compose, GitHub Actions.

## Global Constraints

- Keep `.gitignore` rule `internal/shim/embedded/ucd-sh-*`.
- Do not track generated shims or zero-byte placeholders.
- Placeholder preparation only creates missing files and never overwrites a real shim.

---

### Task 1: Prepare ignored embed inputs before tests

**Files:** Create `scripts/prepare-shim-placeholders.sh`; modify `Makefile` and `.github/workflows/ci.yml`; delete `internal/shim/embedded/ucd-sh-amd64` from Git.

- [ ] **Step 1: Write the preparation script**

```bash
#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
embedded="$root/internal/shim/embedded"
mkdir -p "$embedded"
for arch in amd64 arm64; do
  path="$embedded/ucd-sh-$arch"
  if [[ ! -e "$path" ]]; then : > "$path"; fi
done
```

- [ ] **Step 2: Verify the script is non-destructive**

```bash
rm -f internal/shim/embedded/ucd-sh-amd64 internal/shim/embedded/ucd-sh-arm64
scripts/prepare-shim-placeholders.sh
printf sentinel > internal/shim/embedded/ucd-sh-amd64
scripts/prepare-shim-placeholders.sh
test "$(cat internal/shim/embedded/ucd-sh-amd64)" = sentinel
```

- [ ] **Step 3: Prepare before compiling Go**

Add `./scripts/prepare-shim-placeholders.sh` before every `go test` recipe in Make and CI. Replace the tracked-zero-byte CI assertion with `test -z "$(git ls-files -- internal/shim/embedded/ucd-sh-amd64 internal/shim/embedded/ucd-sh-arm64)"`.

- [ ] **Step 4: Verify and commit**

```bash
git rm --cached internal/shim/embedded/ucd-sh-amd64
git check-ignore internal/shim/embedded/ucd-sh-amd64 internal/shim/embedded/ucd-sh-arm64
make test-short
git add Makefile .github/workflows/ci.yml scripts/prepare-shim-placeholders.sh
git commit -m "build: prepare ignored shim placeholders for tests"
```

### Task 2: Isolate Air shim generation from the source mount

**Files:** Modify `.air.agent.toml`, `docker-compose.yaml`, and `docs/agents.md`.

- [ ] **Step 1: Replace Air's build command**

Set `build.cmd` to copy `/app` excluding `.git`, `tmp`, and both shim paths into `/tmp/unified-cd-agent-src`; run the preparation script and build the real shim there; then build the agent to `/app/tmp/unified-agent`.

- [ ] **Step 2: Document and verify the contract**

Document that Compose uses a disposable source copy and does not mutate shim paths in the bind mount. Verify with `docker compose run --rm agent`, `test -x tmp/unified-agent`, `git status --short`, and `git diff --check`.

- [ ] **Step 3: Commit**

```bash
git add .air.agent.toml docker-compose.yaml docs/agents.md
git commit -m "build: isolate Air shim generation from source"
```
