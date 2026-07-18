package agent

import (
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
	t.Setenv("UNIFIED_AGENT_TOKEN", "super-secret")
	t.Setenv("UNIFIED_CACHE_KEY", "ck")
	t.Setenv("UNIFIED_CACHE_SECRET", "cs")
	t.Setenv("UNIFIED_TOKEN", "ut")
	t.Setenv("UNIFIED_AGENT_CREDENTIAL_FILE", "/var/lib/ucd/credentials.json")
	t.Setenv("UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE", "/var/lib/ucd/enrollment")

	got := envMap(t, StepEnv(nil, nil))
	for _, banned := range []string{
		"UNIFIED_AGENT_TOKEN", "UNIFIED_CACHE_KEY", "UNIFIED_CACHE_SECRET",
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

func TestStepEnv_ExposeEnvAllowlisted(t *testing.T) {
	t.Setenv("MY_BUILD_FLAG", "on")
	t.Setenv("NOT_LISTED", "nope")

	got := envMap(t, StepEnv([]string{"MY_BUILD_FLAG"}, nil))
	assert.Equal(t, "on", got["MY_BUILD_FLAG"])
	assert.NotContains(t, got, "NOT_LISTED")
}

func TestStepEnv_DenylistBeatsExposeEnv(t *testing.T) {
	t.Setenv("UNIFIED_AGENT_TOKEN", "super-secret")
	// An operator must not be able to foot-gun a credential into steps.
	got := envMap(t, StepEnv([]string{"UNIFIED_AGENT_TOKEN"}, nil))
	assert.NotContains(t, got, "UNIFIED_AGENT_TOKEN")
}

func TestStepEnv_ExtraEnvWins(t *testing.T) {
	t.Setenv("MY_BUILD_FLAG", "from-host")
	got := envMap(t, StepEnv([]string{"MY_BUILD_FLAG"}, []string{"MY_BUILD_FLAG=from-step"}))
	assert.Equal(t, "from-step", got["MY_BUILD_FLAG"])
}
