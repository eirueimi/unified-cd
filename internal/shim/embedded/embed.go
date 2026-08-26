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
// builds with -buildvcs=false -trimpath CGO_ENABLED=0.
//
// REGENERATE ON LINUX ONLY. This repo is developed on Windows as well as
// Linux, and a Windows-hosted build of these same linux/amd64 and
// linux/arm64 targets was measured to differ substantially from a
// Linux-hosted one, byte for byte — cross-host Go build reproducibility is
// not guaranteed even for an identical Go version and identical
// -trimpath -buildvcs=false flags. Regenerating on Windows (or macOS) will
// produce bytes CI's freshness check (below) rejects. Use the go.mod-pinned
// toolchain on Linux, e.g. via Docker from the repo root:
//
//	docker run --rm -v "$PWD:/work" -w /work golang:1.26.2 go generate ./internal/shim/embedded/...
//
// (match the image tag to go.mod's `go` directive).
//
// The committed bytes ARE required to be byte-identical to a fresh rebuild
// on that canonical environment (Linux, go.mod's pinned Go version, these
// exact flags) — CI's "Shim binary freshness" job (.github/workflows/ci.yml)
// enforces this with a byte-exact `git diff` after regenerating. This is
// narrower than "Go builds are reproducible in general" and was verified
// empirically for this specific build before relying on it, not assumed:
// repeated builds on the same host, builds from different absolute source
// checkout paths, builds with GOROOT relocated to a different path, and
// builds under both a CPU-count-constrained container and an explicit
// non-default GOMAXPROCS all produced byte-identical output on Linux with
// the pinned toolchain — despite the binaries containing full DWARF debug
// sections, which is where Go's own historical concurrent-compilation
// nondeterminism (golang/go#38068) would have shown up. Only the host OS
// (Windows vs Linux) produced a difference, which is why the CI job runs on
// Linux only and this doc says the same.
//
// embed_test.go separately validates the committed files functionally —
// each is a real, statically-linked linux ELF of the expected architecture,
// and on a linux host the embedded shim is executed and must behave as
// ucd-sh. That check runs everywhere (including Windows/macOS `go test`, and
// CI's own unit-test jobs) and is not replaced by the freshness check above:
// it catches a corrupted or truncated committed file, which a byte-diff
// against a regeneration would not distinguish from ordinary drift. Between
// the two, a source change to cmd/ucd-sh left un-regenerated is now caught
// by CI (the freshness job), not just left as a silent trap. Because the
// bytes are committed, `go build`, `go test`, `go install
// .../cmd/unified-cd-agent@version`, container builds, and goreleaser all
// embed the shim with no pre-build step.
package embedded

// Bytes returns the embedded, committed ucd-sh binary for the architecture
// this package was compiled for (see embed_amd64.go / embed_arm64.go). It is
// always non-empty in a correct checkout; a zero length means the committed
// ucd-sh-<arch> file was truncated or lost and must be regenerated with
// `go generate ./internal/shim/embedded/`.
func Bytes() []byte {
	return payload
}
