package runtime

import (
	"fmt"
	"os"
	"strings"
)

// Step env values (which may be resolved secrets) must never appear on the
// `container` command line, where any local user can read them from the
// process list. Apple's CLI is invoked with `--env-file <path>` (documented
// for `container run` and `container exec`) pointing at a 0600 temporary file
// that is removed once the command has finished. Unlike docker/podman the
// `-e NAME` inherit-from-environment form is not relied on, as the Apple CLI
// does not document it.

// appleEnvFile writes env to a 0600 temporary file and returns the extra argv
// (`--env-file <path>`) to place right after the subcommand, plus a cleanup
// func. With no env entries it returns no args and a no-op cleanup. A bare
// NAME entry is resolved from the agent's own environment (skipped when
// unset), matching docker's `-e NAME` semantics.
func appleEnvFile(env []string) (args []string, cleanup func(), err error) {
	cleanup = func() {}
	var lines []string
	for _, e := range env {
		if !strings.Contains(e, "=") {
			v, ok := os.LookupEnv(e)
			if !ok {
				continue
			}
			e = e + "=" + v
		}
		if strings.ContainsAny(e, "\r\n") {
			k, _, _ := strings.Cut(e, "=")
			return nil, cleanup, fmt.Errorf("env %q has a value containing a newline, which cannot be passed safely to the container CLI", k)
		}
		lines = append(lines, e)
	}
	if len(lines) == 0 {
		return nil, cleanup, nil
	}
	// os.CreateTemp creates the file with mode 0600.
	f, err := os.CreateTemp("", "ucd-env-*")
	if err != nil {
		return nil, cleanup, fmt.Errorf("create env file: %w", err)
	}
	name := f.Name()
	cleanup = func() { _ = os.Remove(name) }
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		_ = f.Close()
		cleanup()
		return nil, func() {}, fmt.Errorf("write env file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("close env file: %w", err)
	}
	return []string{"--env-file", name}, cleanup, nil
}

// withAppleEnvFile inserts extra after the subcommand (args[0]).
func withAppleEnvFile(args, extra []string) []string {
	if len(extra) == 0 {
		return args
	}
	out := make([]string, 0, len(args)+len(extra))
	out = append(out, args[0])
	out = append(out, extra...)
	return append(out, args[1:]...)
}
