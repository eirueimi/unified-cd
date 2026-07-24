package agent

import (
	"runtime"
	"strings"
	"testing"

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
		wantVars = []string{"APPDATA", "LOCALAPPDATA"}
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
