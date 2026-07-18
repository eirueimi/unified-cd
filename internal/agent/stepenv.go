package agent

import (
	"os"
	"runtime"
	"strings"
)

// stepEnvDenied lists environment variables that must NEVER reach a job step,
// even if an operator names them in ExposeEnv. These are the agent's own
// credentials: leaking them lets any job author act as the agent (and, via the
// cache credentials, write directly to the shared object store, bypassing every
// controller-side check).
//
// UNIFIED_AGENT_CREDENTIAL_FILE and UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE (see
// internal/config/agent.go) are listed for completeness with "any per-agent
// credential #63 introduced" rather than as a real control: they name
// filesystem paths, not secret values, and a native step already runs as the
// same OS user as the agent, so it can read the file at that path directly
// regardless of whether the env var naming it is exposed. Denying them here
// costs nothing and keeps this list matching the spec's stated scope.
var stepEnvDenied = map[string]bool{
	"UNIFIED_AGENT_TOKEN":                 true,
	"UNIFIED_CACHE_KEY":                   true,
	"UNIFIED_CACHE_SECRET":                true,
	"UNIFIED_TOKEN":                       true,
	"UNIFIED_CONTROLLER_KEY":              true,
	"UNIFIED_AGENT_CREDENTIAL_FILE":       true,
	"UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE": true,
}

// stepEnvBaseline returns the environment variable names a shell needs to
// function at all. Everything else must be opted in via ExposeEnv.
func stepEnvBaseline() []string {
	if runtime.GOOS == "windows" {
		return []string{"PATH", "PATHEXT", "SystemRoot", "SystemDrive", "COMSPEC", "TEMP", "TMP", "USERPROFILE"}
	}
	return []string{"PATH", "HOME", "PWD", "SHELL", "TMPDIR", "LANG", "LC_ALL", "TZ", "USER"}
}

// StepEnv builds the environment for a job step. It deliberately does NOT
// inherit the agent's process environment (see stepEnvDenied): the agent's env
// holds fleet credentials, and a step is authored by a job author we do not
// trust with them. The k8s agent already builds a fresh env this way
// (imageStepEnv); this is the host-side equivalent.
//
// Precedence, lowest to highest: OS baseline -> ExposeEnv allowlist -> extraEnv
// (the orchestrator's already-expanded step env). Denied names are dropped at
// every layer except extraEnv, which the controller — not the job author —
// controls.
func StepEnv(exposeEnv []string, extraEnv []string) []string {
	out := make([]string, 0, len(extraEnv)+16)
	seen := map[string]bool{}

	add := func(name string) {
		if name == "" || seen[name] || stepEnvDenied[name] {
			return
		}
		v, ok := os.LookupEnv(name)
		if !ok {
			return
		}
		seen[name] = true
		out = append(out, name+"="+v)
	}

	for _, name := range stepEnvBaseline() {
		add(name)
	}
	for _, name := range exposeEnv {
		add(strings.TrimSpace(name))
	}
	// extraEnv wins: append last so a duplicate key overrides earlier entries
	// (os/exec uses the last occurrence).
	out = append(out, extraEnv...)
	return out
}
