// Package embedded holds the ucd-sh binary that the host agent injects into
// every job container it creates, at /.ucd/ucd-sh (see
// docs/superpowers/specs/2026-07-12-step-shell-shim-design.md, Component 2
// "Distribution" and Component 3 "/.ucd injection").
//
// The shim itself always targets linux (job containers share the host
// arch, not the host OS), but the agent binary that embeds it ships for
// multiple OSes and CPU architectures (see .goreleaser.yaml:
// linux/darwin/windows x amd64/arm64). Which linux ucd-sh gets baked in is
// selected by the COMPILING GOARCH via Go build tags, not by the target
// OS: a windows/amd64 agent embeds the linux/amd64 shim (ucd-sh-amd64); a
// darwin/arm64 or linux/arm64 agent embeds the linux/arm64 shim
// (ucd-sh-arm64). Cross-compiling for a given GOARCH automatically picks up
// that arch's file, so `internal/shim/embedded/embed_amd64.go` (`//go:build
// amd64`) and `embed_arm64.go` (`//go:build arm64`) each define the
// package-level `payload` var via a single-file `//go:embed`, and this
// file only exposes the shared `Bytes()` accessor. Only one arch's bytes
// are ever compiled into a given binary.
//
// internal/shim/embedded/ucd-sh-amd64 and ucd-sh-arm64 are BUILD-PRODUCED
// artifacts, not source. The copies committed to git are intentional
// zero-byte placeholders: go:embed cannot reference a path that doesn't
// exist at all, so leaving either file out (and git-ignoring it) would
// break `go build ./...` on a fresh clone or in plain CI, before the
// two-stage build step (`make build`/`make embed-shim`, the
// `.air.agent.toml` dev-loop cmd, the `docker/agent.Dockerfile` builder
// stage, or `scripts/build-shims.sh` invoked from .goreleaser.yaml's
// `before.hooks`) has had a chance to run and overwrite the relevant one
// with a real linux ucd-sh binary. Committed empty files compile cleanly
// via go:embed in every one of those contexts; only the byte length of
// Bytes() tells you whether the real shim made it in.
//
// IMPORTANT — do not commit real bytes here: running the two-stage build
// locally (`make embed-shim`/`make build`, or `air` under .air.agent.toml)
// overwrites the host-arch file with an actual binary. That is expected
// for local dev and container builds, but the files checked into git must
// always stay empty placeholders. Before committing, run `make
// check-embed-clean` (which also verifies the git-blob size at HEAD is
// zero for both files, matching the CI guard in .github/workflows/ci.yml)
// and make sure it reports no change.
package embedded

// Bytes returns the embedded ucd-sh binary for the architecture this
// package was compiled for (see embed_amd64.go / embed_arm64.go).
//
// A zero-length result means the shim was never built into this binary —
// the committed placeholder was not overwritten by the two-stage build
// before compilation. Callers that need the shim to actually be present
// (the host agent, at startup, to populate its tools dir — see Task 5) must
// treat len(Bytes()) == 0 as a hard startup error with an actionable
// message (e.g. pointing at `make build` / the two-stage build step), not
// silently proceed without the shim.
func Bytes() []byte {
	return payload
}
