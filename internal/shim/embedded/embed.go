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
// builds with -buildvcs=false -trimpath CGO_ENABLED=0. Prefer regenerating
// on Linux: a Windows-hosted build of these same linux/amd64 and
// linux/arm64 targets was measured to differ substantially, byte for byte,
// from a Linux-hosted one, so a Windows regeneration is not wrong but does
// churn the diff more than necessary. Via Docker from the repo root
// (match the image tag to go.mod's `go` directive):
//
//	docker run --rm -v "$PWD:/work" -w /work golang:1.26.2 go generate ./internal/shim/embedded/...
//
// The committed bytes are NOT required to be byte-identical to a fresh
// rebuild, and a byte-exact `git diff` guard is unworkable — this was tried
// (see the git history of this file and .github/workflows/ci.yml around
// PR #157) and reverted after failing on two independent, verified grounds:
//
//   - File mode. This repo is developed on Windows, where `core.filemode`
//     is `false`; a commit made here always records mode 100644 for these
//     files regardless of the actual bytes' executable bit, forever. A
//     `go build` on Linux produces an executable (mode 100755). A
//     `git diff --exit-code` guard on a Linux CI runner therefore
//     mismatches on mode alone, on every run, independent of content —
//     this is not staleness, it recurs by construction and can never be
//     regenerated away from this project's actual development platform.
//   - Content. Even setting mode aside, a rebuild via the go.mod-pinned
//     toolchain (go1.26.2, CGO_ENABLED=0, GOOS=linux, these exact flags) did
//     NOT reproduce byte-for-byte across three independently-checked
//     environments: a local Docker build, a second local Docker build used
//     to probe absolute source path / relocated GOROOT / CPU-count and
//     GOMAXPROCS sensitivity (none of which moved the needle in isolation),
//     and actual GitHub Actions ubuntu-latest via actions/setup-go. All
//     three produced DIFFERENT bytes from each other and from what was
//     already committed, for cmd/ucd-sh source that had not changed. The
//     specific remaining cause was not identified.
//
// In short: Go build reproducibility for this artifact is real but not
// reliable enough to gate CI on, matching this file's original assessment
// before PR #157 attempted otherwise. embed_test.go is the actual coverage
// (below): it validates the committed files functionally — each is a real,
// statically-linked linux ELF of the expected architecture, and on a linux
// host the embedded shim is executed and must behave as ucd-sh. That is all
// `go install` and the release build need. Trade-off: a source change to
// cmd/ucd-sh left un-regenerated is not caught automatically; regenerate
// and commit when you touch the shim. (A platform-independent freshness
// signal that does not depend on build reproducibility — e.g. committing a
// hash of cmd/ucd-sh's source alongside the binary and having CI check it
// against the current source — was proposed in review but not built; see
// PR #157.) Because the bytes are committed, `go build`, `go test`,
// `go install .../cmd/unified-cd-agent@version`, container builds, and
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
