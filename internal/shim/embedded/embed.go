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
// The committed bytes are NOT required to be byte-identical to a fresh
// rebuild: Go builds are not byte-reproducible across build machines (BuildID
// and other environment-derived bytes differ even for the same GOOS/GOARCH and
// Go version), so a byte-exact `git diff` guard is unworkable. Instead,
// embed_test.go validates the committed files functionally — each is a real,
// statically-linked linux ELF of the expected architecture, and on a linux
// host the embedded shim is executed and must behave as ucd-sh. That is all
// `go install` and the release build need. (Trade-off: a source change to
// cmd/ucd-sh left un-regenerated is not caught automatically; regenerate and
// commit when you touch the shim.) Because the bytes are committed, `go build`,
// `go test`, `go install .../cmd/unified-cd-agent@version`, container builds, and
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
