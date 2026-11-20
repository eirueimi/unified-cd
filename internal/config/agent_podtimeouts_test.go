package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentEffective_PodTimeoutsDefaultsEnvFile(t *testing.T) {
	eff, err := AgentEffective("")
	require.NoError(t, err)
	assert.Zero(t, eff.PodPullTimeout, "unset is 0; the agent applies its default")
	assert.Zero(t, eff.PodStartTimeout)

	t.Setenv("UNIFIED_AGENT_POD_PULL_TIMEOUT", "10m")
	t.Setenv("UNIFIED_AGENT_POD_START_TIMEOUT", "2m")
	eff, err = AgentEffective("")
	require.NoError(t, err)
	assert.Equal(t, 10*time.Minute, eff.PodPullTimeout)
	assert.Equal(t, 2*time.Minute, eff.PodStartTimeout)

	p := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(p, []byte("podPullTimeout: 20m\npodStartTimeout: 3m\n"), 0o600))
	eff, err = AgentEffective(p)
	require.NoError(t, err)
	assert.Equal(t, 20*time.Minute, eff.PodPullTimeout, "file beats env")
	assert.Equal(t, 3*time.Minute, eff.PodStartTimeout)
}
