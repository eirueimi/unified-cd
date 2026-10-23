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
	"strconv"
	"strings"

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

	case "statfs":
		if len(args) != 2 {
			usage(stderr)
			return 2
		}
		free, total, err := shim.Statfs(args[1])
		if err != nil {
			// Nothing goes to stdout on failure: the caller parses stdout as
			// numbers, so an error must never look like "free=0 total=0".
			fmt.Fprintf(stderr, "[ucd-sh] statfs: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "free=%d total=%d\n", free, total)
		return 0

	case "fsgroup":
		return runFsGroup(args[1:], stdout, stderr)

	case "chown-as":
		if len(args) < 3 {
			usage(stderr)
			return 2
		}
		uid, gid, ok := parseOwner(args[1])
		if !ok {
			fmt.Fprintf(stderr, "[ucd-sh] chown-as: invalid owner %q (want <uid>:<gid>)\n", args[1])
			return 2
		}
		if err := shim.ChownAs(uid, gid, args[2:]); err != nil {
			fmt.Fprintf(stderr, "[ucd-sh] chown-as: %v\n", err)
			return 1
		}
		return 0

	case "secctx-probe":
		if len(args) != 2 {
			usage(stderr)
			return 2
		}
		if err := shim.Probe(args[1], stdout); err != nil {
			fmt.Fprintf(stderr, "[ucd-sh] secctx-probe: %v\n", err)
			return 1
		}
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
	fmt.Fprintln(w, "usage: ucd-sh -c <script> | ucd-sh pause | ucd-sh statfs <path> | ucd-sh fsgroup [--policy=Always|OnRootMismatch] [--exclude=<path>]... <gid> <dir>... | ucd-sh chown-as <uid>:<gid> <path>... | ucd-sh secctx-probe <dir> | ucd-sh --install <path>")
}

// runFsGroup parses `fsgroup [--policy=Always|OnRootMismatch] [--exclude=<path>]... <gid> <dir>...`.
func runFsGroup(args []string, stdout, stderr io.Writer) int {
	policy := shim.PolicyAlways
	var exclude []string
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		switch {
		case strings.HasPrefix(args[0], "--policy="):
			policy = shim.FsGroupPolicy(strings.TrimPrefix(args[0], "--policy="))
			if policy != shim.PolicyAlways && policy != shim.PolicyOnRootMismatch {
				fmt.Fprintf(stderr, "[ucd-sh] fsgroup: invalid policy %q (want Always or OnRootMismatch)\n", string(policy))
				return 2
			}
		case strings.HasPrefix(args[0], "--exclude="):
			exclude = append(exclude, strings.TrimPrefix(args[0], "--exclude="))
		default:
			fmt.Fprintf(stderr, "[ucd-sh] fsgroup: unknown flag %q\n", args[0])
			return 2
		}
		args = args[1:]
	}
	if len(args) < 2 {
		usage(stderr)
		return 2
	}
	gid, err := strconv.ParseUint(args[0], 10, 31)
	if err != nil {
		fmt.Fprintf(stderr, "[ucd-sh] fsgroup: invalid gid %q\n", args[0])
		return 2
	}
	if err := shim.FsGroup(policy, int(gid), exclude, args[1:], stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "[ucd-sh] fsgroup: %v\n", err)
		return 1
	}
	return 0
}

// parseOwner parses "<uid>:<gid>".
func parseOwner(s string) (uid, gid int, ok bool) {
	u, g, found := strings.Cut(s, ":")
	if !found {
		return 0, 0, false
	}
	ui, err1 := strconv.ParseUint(u, 10, 31)
	gi, err2 := strconv.ParseUint(g, 10, 31)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return int(ui), int(gi), true
}
