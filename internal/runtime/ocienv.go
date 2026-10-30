package runtime

import (
	"fmt"
	"os"
	"strings"
)

// Step env values (which may be resolved secrets) must never appear on the
// docker/podman command line, where any local user can read them from the
// process list. Instead the CLI is invoked with `-e NAME` (no value), which
// both docker and podman resolve from the CLI process's own environment, and
// the value is supplied through cmd.Env of that short-lived child.
//
// Only the names listed with -e are forwarded into the container, so the
// agent's own environment (which the child inherits) does not leak in.
//
// A step env key that the CLI itself consumes (PATH, HOME, DOCKER_HOST,
// CONTAINER_HOST, XDG_RUNTIME_DIR, ...) cannot be set on the child without
// changing how the CLI behaves. Those keys fall back to `--env-file` on a
// 0600 temporary file that is removed once the command has finished.

// cliReservedEnvExact are variable names the docker/podman CLIs read.
var cliReservedEnvExact = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"TMPDIR": true, "TMP": true, "TEMP": true, "TZ": true,
	"XDG_RUNTIME_DIR": true, "DBUS_SESSION_BUS_ADDRESS": true,
	"SSH_AUTH_SOCK": true, "KUBECONFIG": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"SYSTEMROOT": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true,
	"PROGRAMDATA": true, "PATHEXT": true, "COMSPEC": true,
	"LD_LIBRARY_PATH": true, "LD_PRELOAD": true,
}

// cliReservedEnvPrefixes cover whole families of CLI configuration variables.
var cliReservedEnvPrefixes = []string{"DOCKER_", "CONTAINER_", "PODMAN_", "XDG_", "REGISTRY_AUTH_", "BUILDAH_", "NERDCTL_", "CONTAINERD_", "LC_"}

// cliReservedEnv reports whether key must not be set in the CLI child's
// environment.
func cliReservedEnv(key string) bool {
	k := strings.ToUpper(key)
	if cliReservedEnvExact[k] {
		return true
	}
	for _, p := range cliReservedEnvPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// splitStepEnv partitions KEY=VALUE entries. flags holds the `-e NAME`
// arguments (values never included), childEnv the KEY=VALUE entries to add to
// the CLI child environment, and fileEnv the entries that must travel through
// an --env-file because their key collides with a CLI variable.
func splitStepEnv(env []string) (flags, childEnv, fileEnv []string) {
	for _, e := range env {
		k, _, hasValue := strings.Cut(e, "=")
		switch {
		case !hasValue:
			// Bare name: pass-through of the agent's own value, as before.
			flags = append(flags, "-e", e)
		case cliReservedEnv(k):
			fileEnv = append(fileEnv, e)
		default:
			flags = append(flags, "-e", k)
			childEnv = append(childEnv, e)
		}
	}
	return flags, childEnv, fileEnv
}

// envFlags returns only the `-e NAME` arguments for env (no values).
func envFlags(env []string) []string {
	flags, _, _ := splitStepEnv(env)
	return flags
}

// cliEnvPrep carries what a docker/podman invocation needs, beyond the pure
// argv, to deliver a step env without exposing values on the command line.
type cliEnvPrep struct {
	// extraArgs (`--env-file <path>`) go right after the subcommand; empty
	// when no key collided with a CLI variable.
	extraArgs []string
	// childEnv is the full environment for the CLI process, or nil to inherit.
	childEnv []string
	file     string
}

// prepareCLIEnv builds the child environment and, only for colliding keys, a
// 0600 env-file. Call cleanup when the command has finished.
func prepareCLIEnv(env []string) (cliEnvPrep, func(), error) {
	_, childEnv, fileEnv := splitStepEnv(env)
	p := cliEnvPrep{}
	if len(childEnv) > 0 {
		p.childEnv = append(os.Environ(), childEnv...)
	}
	cleanup := func() {}
	if len(fileEnv) > 0 {
		for _, e := range fileEnv {
			if strings.ContainsAny(e, "\r\n") {
				k, _, _ := strings.Cut(e, "=")
				return p, cleanup, fmt.Errorf("env %q collides with a container CLI variable and its value contains a newline, which cannot be passed safely", k)
			}
		}
		f, err := os.CreateTemp("", "ucd-env-*")
		if err != nil {
			return p, cleanup, fmt.Errorf("create env file: %w", err)
		}
		name := f.Name()
		cleanup = func() { _ = os.Remove(name) }
		if _, err := f.WriteString(strings.Join(fileEnv, "\n") + "\n"); err != nil {
			_ = f.Close()
			cleanup()
			return p, func() {}, fmt.Errorf("write env file: %w", err)
		}
		if err := f.Close(); err != nil {
			cleanup()
			return p, func() {}, fmt.Errorf("close env file: %w", err)
		}
		p.file = name
		p.extraArgs = []string{"--env-file", name}
	}
	return p, cleanup, nil
}

// withEnvFile inserts the prep's extra args after the subcommand (args[0]).
func (p cliEnvPrep) withEnvFile(args []string) []string {
	if len(p.extraArgs) == 0 {
		return args
	}
	out := make([]string, 0, len(args)+len(p.extraArgs))
	out = append(out, args[0])
	out = append(out, p.extraArgs...)
	return append(out, args[1:]...)
}
