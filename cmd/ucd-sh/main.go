// Command ucd-sh is the default step-shell interpreter injected into
// unified-cd job containers as /.ucd/ucd-sh. See
// docs/superpowers/specs/2026-07-12-step-shell-shim-design.md (Component 2)
// for the full design.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/eirueimi/unified-cd/internal/shim"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run implements the CLI contract; stdin/stdout/stderr are threaded through
// (rather than hardcoded to the os package) so it can be exercised directly
// in tests without spawning a subprocess.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	switch args[0] {
	case "-c":
		if len(args) != 2 {
			usage(stderr)
			return 2
		}
		code, err := shim.Run(context.Background(), args[1], stdin, stdout, stderr, os.Environ(), "")
		if err != nil {
			fmt.Fprintf(stderr, "[ucd-sh] %v\n", err)
		}
		return code

	case "pause":
		if len(args) != 1 {
			usage(stderr)
			return 2
		}
		shim.Pause()
		return 0

	case "--install":
		if len(args) != 2 {
			usage(stderr)
			return 2
		}
		if err := shim.Install(args[1]); err != nil {
			fmt.Fprintf(stderr, "[ucd-sh] install: %v\n", err)
			return 1
		}
		return 0

	default:
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: ucd-sh -c <script> | ucd-sh pause | ucd-sh --install <path>")
}
