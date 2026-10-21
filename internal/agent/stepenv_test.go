package agent

import (
	"runtime"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, kv := range env {
		parts := strings.SplitN(kv, "=", 2)
		require.Len(t, parts, 2, "malformed env entry %q", kv)
		m[parts[0]] = parts[1]
	}
	return m
}

func TestStepEnv_ExcludesAgentCredentials(t *testing.T) {
	t.Setenv("UNIFIED_CACHE_KEY", "ck")
	t.Setenv("UNIFIED_CACHE_SECRET", "cs")
	t.Setenv("UNIFIED_TOKEN", "ut")
	t.Setenv("UNIFIED_AGENT_CREDENTIAL_FILE", "/var/lib/ucd/credentials.json")
	t.Setenv("UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE", "/var/lib/ucd/enrollment")

	got := envMap(t, StepEnv(nil, nil))
	for _, banned := range []string{
		"UNIFIED_CACHE_KEY", "UNIFIED_CACHE_SECRET",
		"UNIFIED_TOKEN",
		"UNIFIED_AGENT_CREDENTIAL_FILE", "UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE",
	} {
		assert.NotContains(t, got, banned, "%s must never reach a step", banned)
	}
}

func TestStepEnv_KeepsShellBaseline(t *testing.T) {
	got := envMap(t, StepEnv(nil, nil))
	assert.Contains(t, got, "PATH", "a step needs PATH to resolve binaries")
}

// TestStepEnv_BaselineIncludesWellKnownConfigDirs pins that the per-user
// config/data/cache directory variables common toolchains need (Unity, npm,
// dotnet, pip, …) are in the OS baseline, so they work without per-agent
// ExposeEnv. These are non-secret path variables, not credentials.
func TestStepEnv_BaselineIncludesWellKnownConfigDirs(t *testing.T) {
	var wantVars []string
	if runtime.GOOS == "windows" {
		wantVars = []string{"APPDATA", "LOCALAPPDATA", "PROGRAMDATA", "ALLUSERSPROFILE"}
	} else {
		wantVars = []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"}
	}
	for _, k := range wantVars {
		t.Setenv(k, "/some/"+k)
	}
	got := envMap(t, StepEnv(nil, nil))
	for _, k := range wantVars {
		assert.Contains(t, got, k, "%s should be in the OS baseline (non-secret per-user dir)", k)
	}
}

// TestStepEnv_BaselineIncludesIdentityAndCPU pins the non-secret identity,
// CPU-count, and arch variables build tools read (USERNAME/LOGNAME parity with
// USER; NUMBER_OF_PROCESSORS for -j parallelism; PROCESSOR_ARCHITECTURE).
func TestStepEnv_BaselineIncludesIdentityAndCPU(t *testing.T) {
	var wantVars []string
	if runtime.GOOS == "windows" {
		wantVars = []string{"USERNAME", "HOMEDRIVE", "HOMEPATH", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE"}
	} else {
		wantVars = []string{"LOGNAME"}
	}
	for _, k := range wantVars {
		t.Setenv(k, "v-"+k)
	}
	got := envMap(t, StepEnv(nil, nil))
	for _, k := range wantVars {
		assert.Contains(t, got, k, "%s should be in the OS baseline", k)
	}
}

func TestStepEnv_ExposeEnvAllowlisted(t *testing.T) {
	t.Setenv("MY_BUILD_FLAG", "on")
	t.Setenv("NOT_LISTED", "nope")

	got := envMap(t, StepEnv([]string{"MY_BUILD_FLAG"}, nil))
	assert.Equal(t, "on", got["MY_BUILD_FLAG"])
	assert.NotContains(t, got, "NOT_LISTED")
}

func TestStepEnv_DenylistBeatsExposeEnv(t *testing.T) {
	t.Setenv("UNIFIED_CACHE_SECRET", "super-secret")
	// An operator must not be able to foot-gun a credential into steps.
	got := envMap(t, StepEnv([]string{"UNIFIED_CACHE_SECRET"}, nil))
	assert.NotContains(t, got, "UNIFIED_CACHE_SECRET")
}

func TestStepEnv_ExtraEnvWins(t *testing.T) {
	t.Setenv("MY_BUILD_FLAG", "from-host")
	got := envMap(t, StepEnv([]string{"MY_BUILD_FLAG"}, []string{"MY_BUILD_FLAG=from-step"}))
	assert.Equal(t, "from-step", got["MY_BUILD_FLAG"])
}

// A mounted volume's path reaches a step as UNIFIED_VOLUME_<ALIAS>. The name
// comes from the alias, never from the volume the run was handed, and it is
// built by dsl.VolumeEnvName so the agents cannot disagree on it.
func TestStepEnv_ExposesVolumePaths(t *testing.T) {
	extra := volumeExtraEnv(map[string]string{
		"go-cache": "/data/vol/go-cache-2",
		"npm":      "/ws/.npm",
	})
	// Sorted, so a step's environment is stable across runs.
	assert.Equal(t, []string{
		"UNIFIED_VOLUME_GO_CACHE=/data/vol/go-cache-2",
		"UNIFIED_VOLUME_NPM=/ws/.npm",
	}, extra)

	got := envMap(t, StepEnv(nil, extra))
	assert.Equal(t, "/data/vol/go-cache-2", got["UNIFIED_VOLUME_GO_CACHE"])
	assert.Equal(t, "/ws/.npm", got["UNIFIED_VOLUME_NPM"])

	assert.Empty(t, volumeExtraEnv(nil), "a job with no volumes gets no volume variables")
}

// UNIFIED_VOLUME_ names are generated per run, so SynthesizedStepEnv cannot
// list them. Its prefix twin names the rule, and the rule must be one
// apply-time validation enforces too.
func TestSynthesizedStepEnvPrefixes_AreReservedAtApplyTime(t *testing.T) {
	require.NotEmpty(t, SynthesizedStepEnvPrefixes())
	for _, p := range SynthesizedStepEnvPrefixes() {
		assert.Contains(t, dsl.ReservedVarPrefixes(), p,
			"%s prefixes names the orchestrator synthesises but apply-time validation does not reserve it", p)
		probe := p + "ANYTHING"
		assert.Error(t, dsl.ValidateVarKeys(map[string]string{probe: "x"}), probe)
		assert.True(t, varDenied(probe), probe)
		assert.True(t, varDenied(strings.ToLower(probe)), strings.ToLower(probe))
		assert.Empty(t, varsExtraEnv(map[string]string{probe: "x"}), probe)
		assert.Empty(t, varsExtraEnv(map[string]string{strings.ToLower(probe): "x"}), strings.ToLower(probe))
	}
	// The volume names the orchestrator builds are inside the synthesised prefix.
	assert.Contains(t, SynthesizedStepEnvPrefixes(), dsl.VolumeEnvPrefix)
	for _, kv := range volumeExtraEnv(map[string]string{"go-cache": "/x"}) {
		assert.True(t, varDenied(strings.SplitN(kv, "=", 2)[0]), kv)
	}
}

// The other direction: every prefix apply-time validation reserves is one the
// runtime backstop also drops, so a run created before the reservation cannot
// carry a shadowing variable past it.
func TestVarsDenied_CoversEveryReservedPrefix(t *testing.T) {
	require.NotEmpty(t, dsl.ReservedVarPrefixes())
	for _, p := range dsl.ReservedVarPrefixes() {
		probe := p + "SOMETHING"
		assert.True(t, varDenied(probe), "%s is reserved by prefix at apply time but not denied at runtime", probe)
		assert.Empty(t, varsExtraEnv(map[string]string{probe: "x"}))
	}
	assert.False(t, varDenied("UNIFIED_VOLUMES"), "the prefix is UNIFIED_VOLUME_, not a substring match")
	assert.False(t, varDenied("REGISTRY"))
}
