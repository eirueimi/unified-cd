# Docs cleanup + ucd-sh `go generate` / agent `go install` consolidation

**Date:** 2026-07-21
**Status:** Approved (brainstorming)

## Motivation

Three related cleanups requested:

1. Delete the migration guides under `docs/` — they document one-time
   old→new transitions that are now just "how it works".
2. Consolidate agent installation onto
   `go install github.com/eirueimi/unified-cd/cmd/agent@vX.Y.Z`.
3. Consolidate ucd-sh shim generation onto `go generate`.

(2) and (3) are coupled. `cmd/agent` `go:embed`s
`internal/shim/embedded/ucd-sh-<arch>`, but those files are **git-ignored**
(`.gitignore`: `internal/shim/embedded/ucd-sh-*`) and produced only by an
out-of-band build step (`make embed-shim` / `scripts/build-shims.sh`) that is
never committed. Consequences:

- The module zip published to the Go proxy at a tag **omits** the shim files
  entirely (they are untracked). `go install .../cmd/agent@vX.Y.Z` therefore
  fails at compile time on `//go:embed ucd-sh-amd64: no matching files found`.
- `embed.go`'s doc comment and `Makefile`'s `check-embed-clean` describe a
  "committed empty placeholder" scheme that no longer matches reality (the
  files are git-ignored, not committed-empty). This is stale.

The only way to make `go install @version` yield a working agent while keeping
`go:embed` is to **track real shim bytes in git**. That also gives (3) a
natural home: `go generate` produces the committed binaries, exactly like the
existing `schemagen`/`docgen` generators produce committed
`schemas/unified-cd.schema.json` and `docs/field-reference.md`.

## Decisions (from brainstorming)

- **Shim distribution:** commit real linux shim binaries (chosen over
  runtime-fetch or dropping `go install`).
- **Migration docs:** delete the 8 files **and** remove/rewrite every inbound
  link so no dangling references remain.
- **Shim matrix:** only **two** committed binaries — `ucd-sh-amd64`,
  `ucd-sh-arm64`, both `GOOS=linux`. The shim always targets linux (job
  containers share host arch, not host OS); the agent's compile-time `GOARCH`
  selects which via `embed_amd64.go` / `embed_arm64.go`. No windows/darwin
  shim. This keeps the current arch-tagged embed structure unchanged.
- **Binary name:** keep `cmd/agent`; `go install` yields a binary named
  `agent`. Document that name (not `unified-cd-agent`).
- **Drift guard:** yes — a linux CI step regenerates and asserts no diff.
- **Release-time shim:** do **not** regenerate at tag time; add a verify gate
  to `release.yml` that fails the release if committed bytes are stale (keeps
  the `go install` binary and the release tarball byte-identical).
- **Module path:** the real module is `github.com/eirueimi/unified-cd` (the
  request's `github.com/unified-cd/...` does not resolve).

## Part A — Delete migration docs + fix inbound links

### Files to delete (8)

- `docs/migration-2026-07-agent-capability-routing.md`
- `docs/migration-2026-07-host-entrypoint-parity.md`
- `docs/migration-2026-07-job-isolation.md`
- `docs/migration-2026-07-secret-scope-removal.md`
- `docs/migration-2026-07-security-hardening.md`
- `docs/migration-2026-07-step-shell-shim.md`
- `docs/migration-2026-07-uses-jobtemplate.md`
- `docs/migration-agent-auth.md`

### Inbound links to remove or rewrite (known; re-grep during implementation)

Stable user docs referencing the above (line numbers approximate):

| File | Target |
|---|---|
| `docs/agents.md` | migration-agent-auth |
| `docs/authentication.md` | migration-agent-auth |
| `docs/authorization.md` | migration-agent-auth, security-hardening |
| `docs/cli.md` | migration-agent-auth |
| `docs/configuration.md` | security-hardening, migration-agent-auth |
| `docs/getting-started.md` | job-isolation, migration-agent-auth |
| `docs/jobs.md` | job-isolation |
| `docs/kubernetes-integration.md` | host-entrypoint-parity |
| `docs/resources.md` | job-isolation |
| `docs/secrets.md` | security-hardening |

**Rule:** for each link, either drop the trailing "see the migration guide"
sentence, or rewrite it to describe current behavior inline (e.g. an
anchor into a migration guide's error table becomes a one-line statement of
the current rule). No link may point at a deleted file.

**Out of scope:** `docs/superpowers/plans/**` and `.superpowers/**` are
historical planning artifacts — leave their prose references untouched.

**Verification:** after edits,
`grep -rn "migration-2026\|migration-agent-auth" docs/*.md README.md` returns
nothing (excluding `docs/superpowers/**`), and a repo-wide markdown
dead-link scan of the top-level docs is clean.

## Part B — ucd-sh via `go generate`, real bytes committed

Adopt the same "generate → commit → (CI guards freshness)" model the DSL
schema/docs already use.

### B1. Track the shim files

- Remove `internal/shim/embedded/ucd-sh-*` from `.gitignore`.
- Commit real `internal/shim/embedded/ucd-sh-amd64` and `ucd-sh-arm64`
  (linux, built by the generator below).

### B2. Generator: `cmd/shimgen`

New Go program (portable — no shell, works from Windows dev machines), mirroring
`cmd/schemagen` / `cmd/docgen` conventions:

- Cross-compiles `./cmd/ucd-sh` for `linux/amd64` and `linux/arm64` via
  `exec.Command("go", "build", ...)`.
- **Reproducibility flags are mandatory** — without them the drift guard goes
  red on every commit / every machine:
  - `CGO_ENABLED=0` (child env) — static build, no host libc/linker.
  - `GOOS=linux`, `GOARCH=<amd64|arm64>` (child env).
  - `-trimpath` — strips the builder's absolute module path from the binary.
  - `-buildvcs=false` — **critical.** shimgen builds ucd-sh from inside the
    git repo, so Go's default `buildvcs=auto` would stamp
    `vcs.revision`/`vcs.time`/`vcs.modified` into the binary, changing the
    bytes on *every commit*. This flag removes that stamp.
- Writes to `internal/shim/embedded/ucd-sh-amd64` and `ucd-sh-arm64`
  (paths resolved relative to the module root, not CWD).
- Deterministic: with the flags above and the go.mod-pinned toolchain, the
  committed bytes change **only** when `cmd/ucd-sh`, its imports
  (notably `internal/shim`), a dependency, or the Go toolchain version
  actually change — exactly what the drift guard should catch.

### B3. `go:generate` directive

Add to `internal/shim/embedded` (e.g. a `generate.go` or the directive block in
`embed.go`):

```go
//go:generate go run github.com/eirueimi/unified-cd/cmd/shimgen
```

`go generate ./...` (and `go generate ./internal/shim/embedded/`) regenerate
both arch binaries.

### B4. Remove obsolete scaffolding

- Delete `scripts/build-shims.sh` and `scripts/prepare-shim-placeholders.sh`.
- `Makefile`: drop `embed-shim` and `check-embed-clean` targets; `build` no
  longer depends on `embed-shim`; `test` / `test-short` / `ha-test` no longer
  call `prepare-shim-placeholders.sh`. Add a `generate:` target
  (`$(GO) generate ./...`) for convenience.
- `.goreleaser.yaml`: remove the `before.hooks: sh scripts/build-shims.sh`
  entry (bytes are committed; the drift guard keeps them fresh). Update the
  now-inaccurate header comment.
- `.air.agent.toml`: drop the tar/copy + `prepare-shim-placeholders.sh` +
  `go build ./cmd/ucd-sh` pre-step; the agent dev build now just
  `go build ./cmd/agent` against the committed bytes. (If a dev edits
  `cmd/ucd-sh`, they run `go generate` manually — same as editing DSL types.)
- `docker/agent.Dockerfile`: drop the `RUN ... go build ... ./cmd/ucd-sh`
  builder line; build the agent directly against committed bytes in the build
  context. Update the two-stage-build comment.

### B5. Rewrite `internal/shim/embedded/embed.go` doc comment

Replace the "empty placeholders, never commit real bytes, two-stage build"
narrative with the new model: the `ucd-sh-<arch>` files are **committed,
generated linux binaries** produced by `go generate` (`cmd/shimgen`); to
refresh them after changing `cmd/ucd-sh`, run `go generate ./...` and commit
the result; CI guards freshness. Keep the `Bytes()` contract and the
arch-tag explanation.

### B6. CI drift guard

In `.github/workflows/ci.yml`:

- Remove the "shim placeholders must be absent from git" step and every
  `./scripts/prepare-shim-placeholders.sh` invocation (unit, integration,
  k8s jobs). Tests now compile against committed bytes with no prep.
- Add a linux-only step (or small job):

  ```bash
  go generate ./internal/shim/embedded/
  git diff --exit-code internal/shim/embedded/ucd-sh-amd64 internal/shim/embedded/ucd-sh-arm64
  ```

  Runs on the go.mod-pinned toolchain so bytes are reproducible.
- The unrelated inline shim build in the `k8s` job (builds a throwaway
  `ucd-shim-test:ci` image) stays.

### B7. Release-time verify gate

`.github/workflows/release.yml` (tag-triggered goreleaser) must **not**
regenerate the shim — the release tarball and `go install @tag` both read the
same committed bytes, so they stay byte-identical. But a tag pushed onto a
commit that never passed the PR/main drift guard could ship stale bytes.

Add a verify (not regenerate) step before goreleaser runs:

```bash
go generate ./internal/shim/embedded/
git diff --exit-code internal/shim/embedded/ucd-sh-amd64 internal/shim/embedded/ucd-sh-arm64
```

If the committed shim is stale at the tag, the release **fails** rather than
silently shipping (or silently rewriting) mismatched bytes. This preserves the
"committed bytes are the single source of truth" invariant.

## Part C — `go install` for the agent

With Part B, `cmd/agent` compiles from the proxy-published module.

- Document the install path in `README.md` and `docs/getting-started.md`:

  ```bash
  go install github.com/eirueimi/unified-cd/cmd/agent@latest   # or @vX.Y.Z
  # installs $GOBIN/agent
  ```

- Note the binary is named `agent` (rename to taste). Existing
  `make build` → `bin/unified-cd-agent` and the release tarballs are
  unaffected and remain documented.
- Scope: only `cmd/agent` was blocked by the embed. `cmd/controller`,
  `cmd/unified-cli`, `cmd/k8s-agent` do not embed the shim and already
  `go install` cleanly; optionally mention them, but no code change is needed.

## Testing / verification

- `go build ./...` and `go test ./... -short` pass on a **fresh clone** with
  no prep step (proves committed bytes suffice).
- `go generate ./internal/shim/embedded/` produces no diff (drift guard green).
- In a clean temp module, `go install github.com/eirueimi/unified-cd/cmd/agent@<local-tag>`
  builds successfully (validated against a local tag / `replace` if needed).
- The started agent reports `len(Bytes()) > 0` (shim present) at startup.
- Docs link scan: no references to deleted migration files outside
  `docs/superpowers/**`.

## Risks / notes

- **Binary reproducibility:** the committed bytes drift only on real changes to
  `cmd/ucd-sh` / `internal/shim` / their deps / the Go toolchain version.
  Spurious drift (VCS stamp, absolute paths, CGO) is eliminated by the B2 build
  flags (`-buildvcs=false -trimpath CGO_ENABLED=0`) — **without `-buildvcs=false`
  the guard reds on every commit.** Cross-Go-version output can still differ, so
  the guard runs on the go.mod-pinned toolchain; local regen is advisory (CI /
  the release gate are source of truth). Documented in the `embed.go` comment.
- **Repo size:** two ~5 MB linux binaries enter git history and grow on every
  `cmd/ucd-sh` change. Accepted tradeoff for a working `go install`.
- **Existing convention:** schemagen/docgen have no CI drift guard; we add one
  here because a stale binary is far harder to spot in review than stale text.
