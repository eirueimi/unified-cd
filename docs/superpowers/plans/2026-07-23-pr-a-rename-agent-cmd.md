# PR A — Rename `cmd/agent` → `cmd/unified-cd-agent` — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Rename the agent command directory `cmd/agent` → `cmd/unified-cd-agent` and update every reference, so `go install …/cmd/unified-cd-agent`, `make build`, releases, and docs all converge on the binary name `unified-cd-agent`. No behavior change.

**Architecture:** `cmd/agent` is a thin `package main` wrapper around `internal/agent`. Only the `cmd` wrapper moves; `internal/agent` (the library) does NOT move. This is a directory move plus reference updates (build config, docker, air, docs, code comments).

**Tech Stack:** Go 1.26.2, goreleaser, Make, Docker, Markdown.

## Global Constraints

- Module path `github.com/eirueimi/unified-cd`.
- No cgo/gcc → run Go tests without `-race`.
- Behavior must be identical — this PR only moves files and updates references.
- **Repo-wide scan gate:** after the change, `grep -rn "cmd/agent\b" . --include=*.go --include=*.md --include=*.yaml --include=*.yml --include=*.toml --include=*.Dockerfile --include=Makefile | grep -v "docs/superpowers/" | grep -v vendor/ | grep -v "\.superpowers/" | grep -v "cmd/k8s-agent"` must return NOTHING (all `cmd/agent` became `cmd/unified-cd-agent`; `cmd/k8s-agent`, `internal/agent`, `bin/unified-cd-agent` are unrelated and stay).
- Commit messages end with `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.
- Run from repo root `C:\Users\arimax\unified-cd-project\unified-cd`; prefix Bash with `cd /c/Users/arimax/unified-cd-project/unified-cd && …` (working dir can reset).

---

### Task 1: Move the directory and update all references

**Files:**
- Move: `cmd/agent/main.go` → `cmd/unified-cd-agent/main.go`; `cmd/agent/main_test.go` → `cmd/unified-cd-agent/main_test.go`
- Modify (functional build/config): `.goreleaser.yaml`, `Makefile`, `docker/agent.Dockerfile`, `.air.agent.toml`, `README.md`, `docs/getting-started.md`
- Modify (comment/doc path references): `docs/agents.md`, `docs/frontend-development.md`, `docs/kubernetes-integration.md`, `docs/operations.md`, `internal/agent/agent.go`, `internal/agent/agent_isolated_test.go`, `internal/agent/claim_pod.go`, `internal/agent/install_shim_test.go`, `internal/shim/embedded/embed.go`, `TODO.md`

**Interfaces:**
- Consumes: nothing new.
- Produces: import path `github.com/eirueimi/unified-cd/cmd/unified-cd-agent`; release/install binary `unified-cd-agent`.

- [ ] **Step 1: Move the directory with git**

Run:
```bash
git mv cmd/agent cmd/unified-cd-agent
```
Expected: `cmd/unified-cd-agent/main.go` and `cmd/unified-cd-agent/main_test.go` exist; `cmd/agent/` is gone. (`package main` inside is unchanged.)

- [ ] **Step 2: Update functional build/config references**

- `.goreleaser.yaml`: line ~49 `main: ./cmd/agent` → `main: ./cmd/unified-cd-agent`; the `agent` build id's `binary: agent` → `binary: unified-cd-agent`. Also the header comment line ~4 `cmd/agent go:embeds …` → `cmd/unified-cd-agent go:embeds …`.
- `Makefile`: line ~15 `$(GO) build $(GOFLAGS) -o bin/unified-cd-agent ./cmd/agent` → `… ./cmd/unified-cd-agent` (output path `bin/unified-cd-agent` stays).
- `docker/agent.Dockerfile`: line ~13 `… go build … -o /agent ./cmd/agent` → `./cmd/unified-cd-agent`; comment lines ~7,12 `cmd/agent` → `cmd/unified-cd-agent`.
- `.air.agent.toml`: line ~10 `cmd = "go build -o /app/tmp/unified-agent ./cmd/agent"` → `./cmd/unified-cd-agent`; comment line ~6 `cmd/agent` → `cmd/unified-cd-agent`.
- `README.md`: line ~82 `go install github.com/eirueimi/unified-cd/cmd/agent@latest       # agent  → $GOBIN/agent` → `…/cmd/unified-cd-agent@latest    # → $GOBIN/unified-cd-agent`.
- `docs/getting-started.md`: line ~40 `go install github.com/eirueimi/unified-cd/cmd/agent@latest` → `…/cmd/unified-cd-agent@latest`; update the surrounding note so the installed binary name is `unified-cd-agent` (no longer the `agent`-vs-`unified-cd-agent` discrepancy — the drift is now resolved).

- [ ] **Step 3: Update comment/doc path references**

Replace `cmd/agent` with `cmd/unified-cd-agent` in the remaining prose/comment sites (each is a path reference, not code): `docs/agents.md:387`, `docs/frontend-development.md:95`, `docs/kubernetes-integration.md:32`, `docs/operations.md` (181, 213, 221 — incl. `cmd/agent/main.go`, `cmd/agent/main_test.go`), `internal/agent/agent.go` (108, 561, 701), `internal/agent/agent_isolated_test.go:258`, `internal/agent/claim_pod.go:225`, `internal/agent/install_shim_test.go:79`, `internal/shim/embedded/embed.go:31`, `TODO.md` (74, 407). Use a careful search-and-replace of the exact substring `cmd/agent` → `cmd/unified-cd-agent` in these files (do NOT touch `cmd/k8s-agent`, `internal/agent`, or `bin/unified-cd-agent`).

- [ ] **Step 4: Build and run the agent-related tests**

Run:
```bash
go build ./...
go test ./cmd/unified-cd-agent/... ./internal/agent/... ./internal/shim/... -count=1
```
Expected: build succeeds; the moved `main_test.go` tests (e.g. `TestDefaultImagesAreDigestPinned`) and internal/agent tests pass. (No `-race`.)

- [ ] **Step 5: Verify `go install` yields the new binary name**

Run:
```bash
tmpbin="$(mktemp -d)"
GOBIN="$tmpbin" GOFLAGS=-mod=mod go install ./cmd/unified-cd-agent
ls -l "$tmpbin"
```
Expected: `$tmpbin/unified-cd-agent` (or `.exe`) exists — proving the install path now produces `unified-cd-agent`.

- [ ] **Step 6: Repo-wide scan must be clean**

Run:
```bash
grep -rn "cmd/agent\b" . --include=*.go --include=*.md --include=*.yaml --include=*.yml --include=*.toml --include=*.Dockerfile --include=Makefile | grep -v "docs/superpowers/" | grep -v vendor/ | grep -v "\.superpowers/" | grep -v "cmd/k8s-agent"
```
Expected: **no output** (every `cmd/agent` became `cmd/unified-cd-agent`).

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "$(printf 'refactor(cmd): rename cmd/agent to cmd/unified-cd-agent for name consistency\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Self-Review

**Spec coverage:** PR A of the design (rename + goreleaser/Makefile/Dockerfile/air/docs/comments) → Task 1 Steps 1-3. Binary-name convergence verified → Step 5. Scan gate → Step 6. ✓

**Placeholder scan:** No TBD/vague steps; every reference site is named with file:line and the exact substring transform.

**Type consistency:** `package main` is unchanged; only the import path (directory) changes. No symbol renames. `git mv` preserves `main_test.go` so existing tests move intact.

**Behavior:** identical — files moved, references updated, no logic touched.
