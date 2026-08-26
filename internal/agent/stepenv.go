package agent

import (
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/eirueimi/unified-cd/internal/dsl"
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
	"UNIFIED_CACHE_KEY":                   true,
	"UNIFIED_CACHE_SECRET":                true,
	"UNIFIED_TOKEN":                       true,
	"UNIFIED_AGENT_CREDENTIAL_FILE":       true,
	"UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE": true,
}

// stepEnvBaseline returns the environment variable names a shell — and the
// common per-user toolchains a job step is likely to invoke — need to function
// at all. Beyond the bare shell essentials it includes the well-known per-user
// config/data/cache directory variables (Windows: APPDATA/LOCALAPPDATA;
// XDG_* on Linux/macOS) that tools such as Unity, npm, dotnet, and pip resolve
// their config/cache from — these are non-secret filesystem paths, not
// credentials, so requiring an operator to opt each one in via ExposeEnv only
// produces confusing "path undefined" failures. Anything else must still be
// opted in via ExposeEnv, and stepEnvDenied always wins.
func stepEnvBaseline() []string {
	if runtime.GOOS == "windows" {
		return []string{
			"PATH", "PATHEXT", "SystemRoot", "SystemDrive", "COMSPEC",
			"TEMP", "TMP", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
			// Machine-wide config/data dir. PROGRAMDATA is where tools keep
			// system-scoped config — e.g. Unity's Package Manager resolves its
			// local config folder as %PROGRAMDATA%\Unity\config and crashes on
			// startup ("path undefined") when it is unset. ALLUSERSPROFILE is
			// the legacy alias for the same directory.
			"PROGRAMDATA", "ALLUSERSPROFILE",
			// Identity, CPU count, and arch — the Windows counterparts of the
			// unix USER/nproc that build tools read (USERNAME parallels USER;
			// NUMBER_OF_PROCESSORS drives MSBuild/cargo/make -j parallelism;
			// HOMEDRIVE+HOMEPATH is how some cross-platform tools construct the
			// home path; PROCESSOR_ARCHITECTURE for arch detection). Non-secret.
			"USERNAME", "HOMEDRIVE", "HOMEPATH", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
		}
	}
	return []string{
		"PATH", "HOME", "PWD", "SHELL", "TMPDIR", "LANG", "LC_ALL", "TZ", "USER",
		"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
		// LOGNAME is the twin of USER that many tools read instead.
		"LOGNAME",
	}
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

// EnvAgentOS and EnvWorkspace are the environment variable names the
// orchestrator synthesises for every step. They are constants, and every site
// that writes them uses these — internal/agent/orchestrator.go's extraEnv and
// internal/k8sagent's imageStepEnv — so SynthesizedStepEnv below really is the
// set, not a description of it.
const (
	EnvAgentOS   = "UNIFIED_AGENT_OS"
	EnvWorkspace = "UNIFIED_WORKSPACE"
)

// SynthesizedStepEnv returns the names the orchestrator itself puts into a
// step's extraEnv. It is the source varsDenied derives from, and the source
// TestVarsDenied_AgreesWithApplyTimeValidation checks dsl.ReservedVarNames
// against — so a name added here without being reserved fails a test rather
// than shipping as a variable that silently overwrites it.
//
// Variables are appended to extraEnv AFTER these, and a later duplicate wins,
// so an unreserved synthesised name is not merely shadowable: it is shadowed
// by any global Vars manifest that happens to use it, for every step of every
// job, on both backends.
func SynthesizedStepEnv() []string {
	return []string{EnvAgentOS, EnvWorkspace}
}

// varsDenied is the set varsExtraEnv filters a claim's variables against: the
// union of stepEnvDenied (the agent's own credentials), every name apply-time
// validation refuses (dsl.ReservedVarNames, which also covers PATH and HOME),
// and every name the orchestrator synthesises (SynthesizedStepEnv). The third
// is folded in by derivation rather than by being listed again here, so a new
// synthesised name is backstopped the moment it is added.
// Keys are upper-cased, and lookups upper-case the candidate.
//
// It is deliberately a separate set rather than a widening of stepEnvDenied,
// because the two govern different things. stepEnvDenied also gates ExposeEnv,
// where an operator names a host variable exactly and exact-case matching may
// be intentional. Variables are the opposite case: their names come from a job
// author's manifest, ValidateVarKeys refuses them case-insensitively, and a
// backstop that refuses less than apply-time does is not a backstop. A run
// created before that validation existed, carrying a global `PATH` — or
// `path`, which is the same variable on Windows — would otherwise replace the
// step's PATH on both backends and break every step of the job with nothing in
// the log to say why.
var varsDenied = buildVarsDenied()

func buildVarsDenied() map[string]bool {
	denied := dsl.ReservedVarNames()
	for k := range stepEnvDenied {
		denied[strings.ToUpper(k)] = true
	}
	for _, k := range SynthesizedStepEnv() {
		denied[strings.ToUpper(k)] = true
	}
	return denied
}

// varsExtraEnv renders a run's variables as KEY=VALUE entries for the
// orchestrator's extraEnv slice, sorted so a step's environment is stable
// across runs with the same inputs.
//
// It applies varsDenied, where StepEnv deliberately applies nothing at all to
// extraEnv. That exemption is correct for the entries the orchestrator itself
// synthesises (UNIFIED_AGENT_OS, UNIFIED_WORKSPACE) because the controller
// controls them. Variables are different: their names and values come from a
// manifest a job author writes. Apply-time validation refuses reserved names
// loudly and early; this is the quiet backstop for a run created before that
// validation existed.
func varsExtraEnv(vars map[string]string) []string {
	if len(vars) == 0 {
		return nil
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		if varsDenied[strings.ToUpper(k)] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+vars[k])
	}
	return out
}
