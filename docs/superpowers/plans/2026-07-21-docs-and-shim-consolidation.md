# Docs cleanup + ucd-sh `go generate` / agent `go install` — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `go install github.com/eirueimi/unified-cd/cmd/agent@vX.Y.Z` produce a working agent by committing real generated linux shim binaries, move shim generation to `go generate`, guard freshness in CI/release, and delete the migration guides (with their inbound links).

**Architecture:** The agent `go:embed`s `internal/shim/embedded/ucd-sh-<arch>`. Today those files are git-ignored and built out-of-band, so the module zip at a tag omits them and `go install` fails to compile. We instead commit two real linux shim binaries produced by a new `cmd/shimgen` (invoked via `go generate`), delete the placeholder/two-stage-build scaffolding, and add a CI drift guard + a release verify gate so the committed bytes stay authoritative and byte-identical across `go install` and release tarballs.

**Tech Stack:** Go 1.26.2, `go:embed`, `go generate`, GitHub Actions, goreleaser v2, air, Docker, Markdown docs.

## Global Constraints

- **Module path:** `github.com/eirueimi/unified-cd` (NOT `github.com/unified-cd`).
- **Shim build flags are mandatory and exact:** `CGO_ENABLED=0`, `GOOS=linux`, `GOARCH=<amd64|arm64>`, `-trimpath`, `-buildvcs=false`. Omitting `-buildvcs=false` makes the drift guard red on every commit (VCS stamp).
- **Exactly two committed shim files:** `internal/shim/embedded/ucd-sh-amd64` and `ucd-sh-arm64`, both `GOOS=linux`. No windows/darwin shim.
- **Committed bytes are the single source of truth.** Release must NOT regenerate — it verifies only. `go install` and the release tarball must embed byte-identical shims.
- **Go toolchain is pinned by `go.mod` (`go 1.26.2`)**; the drift guard and release gate run on that toolchain.
- **`go install .../cmd/agent` yields a binary named `agent`.** Do not rename `cmd/agent`.
- **No dangling links:** after Part A, no reference to a deleted migration file may remain outside `docs/superpowers/**`.
- Every commit message ends with the `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>` trailer.
- Run all commands from the repo root `C:\Users\arimax\unified-cd-project\unified-cd` (a git repo; the outer `unified-cd-project` is not).

---

### Task 1: `cmd/shimgen` generator, committed shim bytes, CI drift guard

Makes committed real bytes the source of truth and keeps CI green. Committing tracked shim bytes immediately breaks the existing ci.yml "must be absent from git" check, so that check's removal + the drift guard belong in this same task.

**Files:**
- Create: `cmd/shimgen/main.go`
- Create: `internal/shim/embedded/generate.go`
- Modify: `.gitignore` (remove the `internal/shim/embedded/ucd-sh-*` line)
- Create (committed, generated): `internal/shim/embedded/ucd-sh-amd64`, `internal/shim/embedded/ucd-sh-arm64`
- Modify: `internal/shim/embedded/embed_test.go` (assert non-empty)
- Modify: `.github/workflows/ci.yml` (remove absent-check + `prepare-shim-placeholders.sh` calls; add drift-guard step)

**Interfaces:**
- Consumes: `./cmd/ucd-sh` (buildable command, imports `internal/shim`).
- Produces:
  - `cmd/shimgen` — a `go run`-able generator that writes the two shim files.
  - `//go:generate go run ../../../cmd/shimgen` in `internal/shim/embedded`.
  - Committed `ucd-sh-amd64` / `ucd-sh-arm64` consumed by `embed_amd64.go` / `embed_arm64.go` (unchanged).

- [ ] **Step 1: Write `cmd/shimgen/main.go`**

Mirrors `cmd/schemagen`'s `projectRoot()` walk so output paths are correct regardless of CWD (go generate sets CWD to the directive's dir, but a direct `go run ./cmd/shimgen` from root must also work).

```go
// Command shimgen cross-compiles cmd/ucd-sh into the two committed linux
// shim binaries that internal/shim/embedded go:embeds
// (internal/shim/embedded/ucd-sh-amd64 and ucd-sh-arm64). It is the
// generator behind that package's //go:generate directive; the produced
// files are committed to git and consumed by go:embed, exactly like
// cmd/schemagen produces schemas/unified-cd.schema.json.
//
// The shim always targets linux (job containers share the host arch, not
// the host OS); the agent's compile-time GOARCH selects which committed
// file is embedded via embed_amd64.go / embed_arm64.go build tags.
//
// Build flags are load-bearing for the CI drift guard: -buildvcs=false
// stops Go stamping the current git revision into the binary (which would
// change the bytes on every commit), -trimpath removes the builder's
// absolute module path, and CGO_ENABLED=0 makes it a static, host-
// independent build.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const ucdShPkg = "github.com/eirueimi/unified-cd/cmd/ucd-sh"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "shimgen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := projectRoot()
	if err != nil {
		return err
	}
	embeddedDir := filepath.Join(root, "internal", "shim", "embedded")

	for _, arch := range []string{"amd64", "arm64"} {
		out := filepath.Join(embeddedDir, "ucd-sh-"+arch)
		cmd := exec.Command("go", "build",
			"-trimpath",
			"-buildvcs=false",
			"-o", out,
			ucdShPkg,
		)
		cmd.Env = append(os.Environ(),
			"CGO_ENABLED=0",
			"GOOS=linux",
			"GOARCH="+arch,
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("build %s: %w", arch, err)
		}
	}
	return nil
}

func projectRoot() (string, error) {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found from %s", dir)
		}
		dir = parent
	}
}
```

- [ ] **Step 2: Add the `go:generate` directive**

Create `internal/shim/embedded/generate.go`:

```go
package embedded

//go:generate go run ../../../cmd/shimgen
```

- [ ] **Step 3: Un-ignore the shim files**

In `.gitignore`, delete the line:

```
internal/shim/embedded/ucd-sh-*
```

- [ ] **Step 4: Generate the shim binaries**

Run: `go generate ./internal/shim/embedded/`
Then verify both are real linux ELF binaries and non-trivial:

Run:
```bash
ls -l internal/shim/embedded/ucd-sh-amd64 internal/shim/embedded/ucd-sh-arm64
# ELF magic (7f 45 4c 46) on both:
head -c 4 internal/shim/embedded/ucd-sh-amd64 | od -An -tx1
head -c 4 internal/shim/embedded/ucd-sh-arm64 | od -An -tx1
```
Expected: both files exist, each > 1 MB, both print ` 7f 45 4c 46`.

- [ ] **Step 5: Verify generation is idempotent (drift-guard precondition)**

Run:
```bash
go generate ./internal/shim/embedded/
git diff --stat internal/shim/embedded/ucd-sh-amd64 internal/shim/embedded/ucd-sh-arm64
```
Expected: no diff on the second run (identical bytes). If a diff appears, a build flag is missing (most likely `-buildvcs=false`) — fix `cmd/shimgen` before continuing.

- [ ] **Step 6: Tighten `embed_test.go` to assert the shim is present**

Committed bytes mean `Bytes()` is now always non-empty for the host arch. Replace the body of `TestBytes` in `internal/shim/embedded/embed_test.go` with:

```go
package embedded

import "testing"

// TestBytes asserts the committed, generated ucd-sh binary for this GOARCH
// is embedded and stable across calls. The bytes are produced by
// `go generate ./internal/shim/embedded/` (cmd/shimgen) and committed to
// git, so a zero length here means the committed file was truncated or the
// wrong file was committed — a regression, not an expected fresh-clone state.
func TestBytes(t *testing.T) {
	b := Bytes()
	if len(b) == 0 {
		t.Fatalf("Bytes() is empty; the committed ucd-sh-<arch> shim is missing or truncated — run `go generate ./internal/shim/embedded/` and commit")
	}
	if len(Bytes()) != len(b) {
		t.Fatalf("Bytes() not stable across calls")
	}
	t.Logf("embedded ucd-sh is %d bytes", len(b))
}
```

- [ ] **Step 7: Run the embedded package test**

Run: `go test ./internal/shim/embedded/ -run TestBytes -v`
Expected: PASS, logs a byte count > 0.

- [ ] **Step 8: Confirm a plain build needs no prep step**

Run: `go build ./...`
Expected: succeeds with no `prepare-shim-placeholders.sh` / `make embed-shim` run first (proves committed bytes satisfy `go:embed`).

- [ ] **Step 9: Update `.github/workflows/ci.yml`**

(a) Remove the whole step that asserts absence from git (the check that now must fail):

```yaml
      - name: shim placeholders must be absent from git
        if: runner.os == 'Linux'
        run: |
          test -z "$(git ls-files -- internal/shim/embedded/ucd-sh-amd64 internal/shim/embedded/ucd-sh-arm64)"
```

(b) In the `unit`, `integration`, and `k8s` jobs, delete the `./scripts/prepare-shim-placeholders.sh` line from each `run:` block (leave the surrounding `go test ...` line intact). The `k8s` job's inline `GOOS=linux GOARCH=amd64 ... go build -o ucd-sh ./cmd/ucd-sh` throwaway-image step **stays**.

(c) Add a drift-guard step to the `unit` job (Linux only, pinned toolchain already set up), after checkout/setup-go:

```yaml
      - name: shim is up to date
        if: runner.os == 'Linux'
        run: |
          go generate ./internal/shim/embedded/
          git diff --exit-code internal/shim/embedded/ucd-sh-amd64 internal/shim/embedded/ucd-sh-arm64
```

- [ ] **Step 10: Verify the drift-guard command locally**

Run:
```bash
go generate ./internal/shim/embedded/ && git diff --exit-code internal/shim/embedded/ucd-sh-amd64 internal/shim/embedded/ucd-sh-arm64 && echo GUARD-GREEN
```
Expected: prints `GUARD-GREEN` (exit 0).

- [ ] **Step 11: Commit**

```bash
git add cmd/shimgen/main.go internal/shim/embedded/generate.go internal/shim/embedded/ucd-sh-amd64 internal/shim/embedded/ucd-sh-arm64 internal/shim/embedded/embed_test.go .gitignore .github/workflows/ci.yml
git commit -m "$(printf 'feat(shim): generate+commit ucd-sh via go generate; CI drift guard\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

### Task 2: Remove obsolete two-stage-build scaffolding

Now-dead placeholder/two-stage machinery. None of it is needed once real bytes are committed; each edit keeps its consumer working against the committed bytes.

**Files:**
- Delete: `scripts/build-shims.sh`
- Delete: `scripts/prepare-shim-placeholders.sh`
- Modify: `Makefile`
- Modify: `.goreleaser.yaml`
- Modify: `docker/agent.Dockerfile`
- Modify: `.air.agent.toml`
- Modify: `internal/shim/embedded/embed.go` (doc comment only)

**Interfaces:**
- Consumes: committed `internal/shim/embedded/ucd-sh-*` and the `go generate` directive from Task 1.
- Produces: no new symbols; `make build`, `docker build`, and the air dev loop all build the agent directly against committed bytes.

- [ ] **Step 1: Delete the shim scripts**

Run:
```bash
git rm scripts/build-shims.sh scripts/prepare-shim-placeholders.sh
```

- [ ] **Step 2: Edit `Makefile`**

Remove the `embed-shim` and `check-embed-clean` targets entirely, drop `embed-shim` from `.PHONY`, drop the `embed-shim` prerequisite from `build`, drop the three `./scripts/prepare-shim-placeholders.sh` lines from `test`, `test-short`, and `ha-test`, and add a `generate` target.

Resulting `build`/`test`/`test-short`/`ha-test`/`generate` targets:

```makefile
build: ui-build
	$(GO) build $(GOFLAGS) -o bin/unified-cd-controller ./cmd/controller
	$(GO) build $(GOFLAGS) -o bin/unified-cd-agent ./cmd/agent
	$(GO) build $(GOFLAGS) -ldflags "-X github.com/eirueimi/unified-cd/internal/cli.version=$(VERSION)" -o bin/unified-cli ./cmd/unified-cli

generate:
	$(GO) generate ./...

test:
	$(GO) test ./... -race -count=1

test-short:
	$(GO) test ./... -short -race -count=1

ha-test:
	cd test/ha && $(GO) test -tags ha -v -timeout 20m ./...
```

Update the `.PHONY` line: remove `embed-shim check-embed-clean`, add `generate`. Also delete the large `embed-shim`/`check-embed-clean` explanatory comment blocks and the `HOSTARCH` comment/variable if now unused (verify `HOSTARCH` has no other reference first: `grep -n HOSTARCH Makefile`).

- [ ] **Step 3: Verify `make build` and tests against committed bytes**

Run:
```bash
make build
git status --porcelain internal/shim/embedded/
```
Expected: `make build` succeeds; `git status` shows **no** modification to the committed shim files (build only reads them). Then:

Run: `go test ./... -short -race -count=1`
Expected: PASS (no prep step needed).

- [ ] **Step 4: Edit `.goreleaser.yaml`**

Remove the `before:` block that runs the shim script:

```yaml
before:
  hooks:
    - sh scripts/build-shims.sh
```

Replace the file's leading comment (lines describing the placeholder/before-hook scheme) with a short note that the `ucd-sh-<arch>` files are committed, generated by `go generate` (`cmd/shimgen`), and freshness is enforced by the CI drift guard and the release verify gate.

- [ ] **Step 5: Edit `docker/agent.Dockerfile`**

Delete the shim-build line:

```dockerfile
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o internal/shim/embedded/ucd-sh-$(go env GOARCH) ./cmd/ucd-sh
```

Leave the agent build line:

```dockerfile
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /agent ./cmd/agent
```

Rewrite the "Two-stage build …" comment (lines 7-12) to state that the committed `internal/shim/embedded/ucd-sh-<arch>` bytes (in the build context) are embedded directly — no pre-build step.

- [ ] **Step 6: Verify the agent image still builds**

Run: `docker build -f docker/agent.Dockerfile -t ucd-agent-buildcheck .`
Expected: build succeeds. (If Docker is unavailable in the environment, note that and rely on Step 3's `go build ./cmd/agent`, which exercises the same embed.)

- [ ] **Step 7: Simplify `.air.agent.toml`**

Replace the `[build].cmd` line and its preceding comment. New `[build]` block (keep every other key exactly as-is):

```toml
[build]
  # The committed linux ucd-sh binaries (internal/shim/embedded/ucd-sh-<arch>,
  # generated by `go generate ./...`) are embedded directly by cmd/agent — the
  # dev-container GOARCH selects the arch via build tag, so no pre-build shim
  # step is needed. If you edit cmd/ucd-sh or internal/shim, run
  # `go generate ./...` and commit before the dev agent will pick it up.
  cmd = "go build -o /app/tmp/unified-agent ./cmd/agent"
  bin = "./tmp/unified-agent"
  full_bin = "./tmp/unified-agent --max-concurrent 2"
  delay = 1000
  include_ext = ["go"]
  exclude_dir = ["vendor", ".git", "tmp", "docs", "deployments", "web", "example", "manifests"]
  kill_delay = "3s"
  rerun = false
  poll = true
  poll_interval = 500
```

(`.air.toml` and `.air.controller.toml` are controller-only — do not touch.)

- [ ] **Step 8: Rewrite `internal/shim/embedded/embed.go` doc comment**

Replace the package doc comment (everything above `package embedded`) so it describes the new model. Keep the `Bytes()` function and its doc updated to match:

```go
// Package embedded holds the ucd-sh binary that the host agent injects into
// every job container it creates, at /.ucd/ucd-sh (see
// docs/superpowers/specs/2026-07-12-step-shell-shim-design.md, Component 2).
//
// The shim always targets linux (job containers share the host arch, not the
// host OS), but the agent binary that embeds it ships for multiple OSes and
// CPU architectures. Which linux ucd-sh gets baked in is selected by the
// COMPILING GOARCH via build tags, not the target OS: a windows/amd64 agent
// embeds ucd-sh-amd64; a darwin/arm64 or linux/arm64 agent embeds
// ucd-sh-arm64. So embed_amd64.go (`//go:build amd64`) and embed_arm64.go
// (`//go:build arm64`) each define `payload` via a single-file `//go:embed`,
// and this file only exposes the shared Bytes() accessor.
//
// internal/shim/embedded/ucd-sh-amd64 and ucd-sh-arm64 are GENERATED,
// COMMITTED linux binaries — build products tracked in git, exactly like
// schemas/unified-cd.schema.json. Regenerate them with
// `go generate ./internal/shim/embedded/` (which runs cmd/shimgen) after
// changing cmd/ucd-sh or internal/shim, and commit the result. cmd/shimgen
// builds with -buildvcs=false -trimpath CGO_ENABLED=0 so the bytes are
// reproducible; a CI drift guard and the release verify gate fail if the
// committed bytes are stale. Because the bytes are committed, `go build`,
// `go test`, `go install .../cmd/agent@version`, container builds, and
// goreleaser all embed the shim with no pre-build step.
package embedded

// Bytes returns the embedded, committed ucd-sh binary for the architecture
// this package was compiled for (see embed_amd64.go / embed_arm64.go). It is
// always non-empty in a correct checkout; a zero length means the committed
// ucd-sh-<arch> file was truncated or lost and must be regenerated with
// `go generate ./internal/shim/embedded/`.
func Bytes() []byte {
	return payload
}
```

- [ ] **Step 9: Verify no dead references to the removed scaffolding remain**

Run:
```bash
grep -rn "prepare-shim-placeholders\|build-shims\|embed-shim\|check-embed-clean" \
  Makefile .goreleaser.yaml docker/ .air.agent.toml .github/ scripts/ 2>/dev/null
```
Expected: no output. Then re-run the embedded test and a full short test:

Run: `go test ./internal/shim/embedded/... -v && go build ./...`
Expected: PASS / success.

- [ ] **Step 10: Commit**

```bash
git add -A
git commit -m "$(printf 'refactor(shim): drop two-stage-build scaffolding; embed committed bytes\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

### Task 3: Release verify gate

Fail a tag release if the committed shim is stale, without regenerating (preserves the byte-identical invariant between `go install` and the release tarball).

**Files:**
- Modify: `.github/workflows/release.yml`

- [ ] **Step 1: Add a verify step before goreleaser**

In `.github/workflows/release.yml`, insert a step after the `actions/setup-go@v5` step and before the `goreleaser/goreleaser-action@v6` step:

```yaml
      - name: shim is up to date (verify, do not regenerate)
        run: |
          go generate ./internal/shim/embedded/
          git diff --exit-code internal/shim/embedded/ucd-sh-amd64 internal/shim/embedded/ucd-sh-arm64
```

- [ ] **Step 2: Verify YAML parses and the command is correct locally**

Run:
```bash
go generate ./internal/shim/embedded/ && git diff --exit-code internal/shim/embedded/ucd-sh-amd64 internal/shim/embedded/ucd-sh-arm64 && echo RELEASE-GATE-OK
```
Expected: prints `RELEASE-GATE-OK`.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "$(printf 'ci(release): verify committed ucd-sh is fresh before goreleaser\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

### Task 4: Delete migration docs and fix inbound links

**Files:**
- Delete: `docs/migration-2026-07-agent-capability-routing.md`, `docs/migration-2026-07-host-entrypoint-parity.md`, `docs/migration-2026-07-job-isolation.md`, `docs/migration-2026-07-secret-scope-removal.md`, `docs/migration-2026-07-security-hardening.md`, `docs/migration-2026-07-step-shell-shim.md`, `docs/migration-2026-07-uses-jobtemplate.md`, `docs/migration-agent-auth.md`
- Modify: `docs/agents.md`, `docs/authentication.md`, `docs/authorization.md`, `docs/cli.md`, `docs/configuration.md`, `docs/getting-started.md`, `docs/jobs.md`, `docs/kubernetes-integration.md`, `docs/resources.md`, `docs/secrets.md`

- [ ] **Step 1: Enumerate every inbound reference (source of truth for this task)**

Run:
```bash
grep -rn "migration-2026-07\|migration-agent-auth" docs README.md | grep -v "docs/superpowers/"
```
Expected: a list confined to the 10 files above (plus the migration files themselves). Treat this output as the checklist — every non-`superpowers` hit must be resolved in Step 3. If a hit appears in a file not listed above, handle it the same way and note it.

- [ ] **Step 2: Delete the 8 migration files**

Run:
```bash
git rm docs/migration-2026-07-agent-capability-routing.md \
       docs/migration-2026-07-host-entrypoint-parity.md \
       docs/migration-2026-07-job-isolation.md \
       docs/migration-2026-07-secret-scope-removal.md \
       docs/migration-2026-07-security-hardening.md \
       docs/migration-2026-07-step-shell-shim.md \
       docs/migration-2026-07-uses-jobtemplate.md \
       docs/migration-agent-auth.md
```

- [ ] **Step 3: Fix each inbound link**

For every hit from Step 1, apply this rule per occurrence:
- If the sentence exists only to point at the migration guide ("See [Migration: …](…)."), delete the sentence (and any now-dangling list-table row).
- If the link is embedded in a sentence stating current behavior, keep the sentence and remove just the parenthetical/anchor link, rewriting so the current rule reads as a plain statement (no "migration"/"old→new" framing).

Concrete edits (verify exact surrounding text with a `Read` before each `Edit`):

- `docs/agents.md`: drop the `See [Migration: agent authentication](migration-agent-auth.md).` sentence.
- `docs/authentication.md`: remove the `[Migration: agent authentication](migration-agent-auth.md)` reference; keep the surrounding guidance as a present-tense statement.
- `docs/authorization.md`: remove both refs (`migration-agent-auth.md`, `migration-2026-07-security-hardening.md`); keep the sentence describing current authz behavior.
- `docs/cli.md`: drop the `See [Migration: agent authentication](migration-agent-auth.md)` clause from the kubeconfig sentence.
- `docs/configuration.md`: in the `UNIFIED_AGENT_EXPOSE_ENV` row, replace the `See [Migration: security hardening](…#…)` link with a plain sentence ("Agent credentials are always dropped and cannot be exposed."); in the token row, drop the `migration-agent-auth.md` reference.
- `docs/getting-started.md`: line ~15, remove the `[job-isolation migration guide](migration-2026-07-job-isolation.md)` link (keep the sentence pointing only at `jobs.md#job-isolation-native-and-the-claim-pod`); line ~436, delete the `| Shared-agent-token migration | [Migration: agent authentication](migration-agent-auth.md) |` table row.
- `docs/jobs.md`: line ~1079, remove the `migration-2026-07-job-isolation.md` link, keep the `podTemplate` mapping sentence.
- `docs/kubernetes-integration.md`: line ~85, remove the `[migration guide](migration-2026-07-host-entrypoint-parity.md)` link; state the host-entrypoint-parity behavior inline.
- `docs/resources.md`: line ~127, drop the `see the [migration guide](migration-2026-07-job-isolation.md)` clause; keep "the old step-level `runsIn: { image / container }` is removed."
- `docs/secrets.md`: line ~186, remove the `migration-2026-07-security-hardening.md` link; keep the secret-fetch-scope statement.

- [ ] **Step 4: Verify no dangling references remain**

Run:
```bash
grep -rn "migration-2026-07\|migration-agent-auth" docs README.md | grep -v "docs/superpowers/"
```
Expected: **no output.**

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "$(printf 'docs: remove migration guides and their inbound links\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

### Task 5: Document `go install` for the agent

**Files:**
- Modify: `docs/getting-started.md`
- Modify: `README.md`

- [ ] **Step 1: Add a `go install` option in getting-started's Build section**

In `docs/getting-started.md` §1 ("Build"), after the `make build` block, add:

```markdown
Alternatively, install the agent binary directly from the module (no clone
needed):

​```bash
go install github.com/eirueimi/unified-cd/cmd/agent@latest   # or @vX.Y.Z
​```

This installs `$(go env GOBIN)/agent` (the binary is named `agent`). The
controller and CLI can be installed the same way
(`go install github.com/eirueimi/unified-cd/cmd/controller@latest`,
`.../cmd/unified-cli@latest`). Elsewhere in this guide the agent is run as
`./bin/unified-cd-agent` (the `make build` output name); substitute `agent`
(or your renamed binary) if you installed it this way.
```

(Remove the zero-width space before each code fence — it is only here to escape the nested block.)

- [ ] **Step 2: Add the install path to `README.md`**

In `README.md`, near the existing `curl … releases/latest/download/unified-cli …` install snippet (around line 75), add a `go install` alternative:

```markdown
Or install from source with Go:

​```bash
go install github.com/eirueimi/unified-cd/cmd/agent@latest       # agent  → $GOBIN/agent
go install github.com/eirueimi/unified-cd/cmd/controller@latest  # controller
go install github.com/eirueimi/unified-cd/cmd/unified-cli@latest # CLI
​```
```

(Remove the zero-width space before each code fence.)

- [ ] **Step 3: Validate `go install` actually works from a tag**

This is the payoff check — that committed bytes make the proxy build succeed. Using an existing or throwaway local tag:

Run:
```bash
# Create a local tag at the current commit (delete after if throwaway):
git tag v0.0.0-shimcheck
# Install from the LOCAL module via a temp GOBIN, resolving the tag from this repo:
tmpbin="$(mktemp -d)"
GOBIN="$tmpbin" GOFLAGS=-mod=mod go install ./cmd/agent
ls -l "$tmpbin"
```
Expected: `$tmpbin/agent` (or `agent.exe`) exists and is a multi-MB binary — proving `./cmd/agent` builds with the embedded shim from committed bytes. (A true `@version` proxy install requires the tag to be pushed; the local `go install ./cmd/agent` exercises the same embed + compile path. Note this in the task report.) Clean up: `git tag -d v0.0.0-shimcheck`.

- [ ] **Step 4: Commit**

```bash
git add docs/getting-started.md README.md
git commit -m "$(printf 'docs: document go install for the agent (committed shim makes it build)\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Self-Review

**Spec coverage:**
- Part A (delete migration docs + fix links) → Task 4. ✓
- Part B1 (un-ignore + commit real bytes) → Task 1 Steps 3-4. ✓
- Part B2 (`cmd/shimgen`, mandatory flags incl. `-buildvcs=false`) → Task 1 Step 1. ✓
- Part B3 (`go:generate` directive) → Task 1 Step 2. ✓
- Part B4 (remove scaffolding: scripts, Makefile, goreleaser, Dockerfile, air) → Task 2 Steps 1-7. ✓
- Part B5 (rewrite embed.go doc) → Task 2 Step 8. ✓
- Part B6 (CI: remove placeholder steps, add drift guard) → Task 1 Step 9. ✓
- Part B7 (release verify gate) → Task 3. ✓
- Part C (go install docs, `agent` name, module path) → Task 5. ✓
- Global: embed_test.go tightening (implied by committed-always-present) → Task 1 Step 6. ✓

**Placeholder scan:** No "TBD"/"handle edge cases"/"similar to Task N"; all code and commands are literal. The zero-width-space note in Task 5 is an intentional escaping instruction, not a placeholder.

**Type consistency:** `cmd/shimgen`'s `run()`/`projectRoot()` mirror `cmd/schemagen`'s names. The `go:generate` path `../../../cmd/shimgen` matches the `internal/shim/embedded` → repo-root depth. Committed filenames `ucd-sh-amd64`/`ucd-sh-arm64` match `embed_amd64.go`/`embed_arm64.go`'s `//go:embed` targets. Build flags are identical everywhere they appear (`cmd/shimgen`, embed.go doc, CI/release gates).

**Ordering note:** Task 1 must land before Task 2 (Task 2's cleanup assumes committed bytes exist) and before Task 3/Task 5 (both depend on the committed shim + generate directive). Tasks 4 (docs) is independent and may run in any order.
