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
