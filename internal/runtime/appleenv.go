package runtime

import (
	"fmt"
	"os"
	"strings"
)

// Step env values (which may be resolved secrets) must never appear on the
// `container` command line, where any local user can read them from the
// process list. Two channels are used, both supported by Apple's CLI
// (apple/container Parser.allEnv / Parser.env):
//
//   - `--env-file <path>`: a 0600 temporary file removed once the command has
//     finished. The file is line-based, so it cannot carry a value that
//     contains a newline.
//   - `-e NAME` with no value, which the CLI resolves from its own environment;
//     the value is supplied through cmd.Env of the short-lived child. Used for
//     values that contain a newline (PEM keys, kubeconfigs). The name must not
//     be one the CLI itself consumes (see cliReservedEnv); such a value cannot
//     be passed safely and is an error.
//
// `-e NAME` needs a `container` release that inherits bare names from the
// environment, so it is only used where the env-file cannot carry the value.

// appleEnvPrep is what an Apple `container` invocation needs to deliver a step
// env without exposing values on the command line.
type appleEnvPrep struct {
	// extraArgs go right after the subcommand: `--env-file <path>` and
	// `-e NAME` flags. Never contain a value.
	extraArgs []string
	// childEnv is the full environment for the CLI process, or nil to inherit.
	childEnv []string
}

// prepareAppleEnv splits env into the env-file and the inherit-by-name channel.
// Call cleanup when the command has finished. A bare NAME entry is resolved
// from the agent's own environment (skipped when unset), matching docker's
// `-e NAME` semantics.
func prepareAppleEnv(env []string) (appleEnvPrep, func(), error) {
	cleanup := func() {}
	var p appleEnvPrep
	var lines, inherit []string
	for _, e := range env {
		if !strings.Contains(e, "=") {
			v, ok := os.LookupEnv(e)
			if !ok {
				continue
			}
			e = e + "=" + v
		}
		if !strings.ContainsAny(e, "\r\n") {
			lines = append(lines, e)
			continue
		}
		k, _, _ := strings.Cut(e, "=")
		if cliReservedEnv(k) {
			return p, cleanup, fmt.Errorf("env %q collides with a container CLI variable and its value contains a newline, which cannot be passed safely", k)
		}
		inherit = append(inherit, e)
	}
	for _, e := range inherit {
		k, _, _ := strings.Cut(e, "=")
		p.extraArgs = append(p.extraArgs, "-e", k)
		if p.childEnv == nil {
			p.childEnv = os.Environ()
		}
		p.childEnv = append(p.childEnv, e)
	}
	if len(lines) == 0 {
		return p, cleanup, nil
	}
	// os.CreateTemp creates the file with mode 0600.
	f, err := os.CreateTemp("", "ucd-env-*")
	if err != nil {
		return p, cleanup, fmt.Errorf("create env file: %w", err)
	}
	name := f.Name()
	cleanup = func() { _ = os.Remove(name) }
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		_ = f.Close()
		cleanup()
		return p, func() {}, fmt.Errorf("write env file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return p, func() {}, fmt.Errorf("close env file: %w", err)
	}
	p.extraArgs = append([]string{"--env-file", name}, p.extraArgs...)
	return p, cleanup, nil
}

// withEnv inserts the prep's extra args after the subcommand (args[0]).
func (p appleEnvPrep) withEnv(args []string) []string {
	if len(p.extraArgs) == 0 {
		return args
	}
	out := make([]string, 0, len(args)+len(p.extraArgs))
	out = append(out, args[0])
	out = append(out, p.extraArgs...)
	return append(out, args[1:]...)
}
