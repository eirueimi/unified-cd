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
// -buildvcs=false stops Go stamping the current git revision into the
// binary (which would change the bytes on every commit for reasons that
// have nothing to do with the shim itself), -trimpath removes the
// builder's absolute module path, and CGO_ENABLED=0 makes it a static
// build with no dynamic-loader dependency. These keep the two binaries as
// close to reproducible as Go's toolchain allows, but not exactly
// reproducible — see internal/shim/embedded/embed.go's package doc for why
// that gap is real and why CI does not gate on byte-exact output. What CI
// (and `go test`) DOES gate on is freshness of the shim's SOURCE: after
// building, run also writes a hash of cmd/ucd-sh's and internal/shim's
// current source (internal/shim/srchash) to
// internal/shim/embedded/ucd-sh-source.sha256, which
// internal/shim/embedded's TestShimSourceMatchesRecordedHash recomputes and
// compares against on every `go test`. Commit that file alongside the
// binaries; there is no separate manual step to remember.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/eirueimi/unified-cd/internal/shim/srchash"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "shimgen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := srchash.FindModuleRoot(wd)
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
			srchash.ShimPackage,
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

	// Record what source produced these binaries. This is the freshness
	// signal internal/shim/embedded's TestShimSourceMatchesRecordedHash
	// checks on every `go test` — see srchash's package doc for what goes
	// into the hash and why it is computed from source rather than from
	// these binaries' own (non-reproducible) bytes. Writing it here, as
	// part of the same generate step that produces the binaries, is what
	// makes regeneration a single command: a developer who edits the shim
	// and runs `go generate` gets an up-to-date hash for free, with no
	// second step to forget.
	hash, err := srchash.Compute(root)
	if err != nil {
		return fmt.Errorf("compute shim source hash: %w", err)
	}
	hashPath := filepath.Join(root, srchash.RecordedHashPath)
	if err := os.WriteFile(hashPath, []byte(hash+"\n"), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", hashPath, err)
	}

	return nil
}
