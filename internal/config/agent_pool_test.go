package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentEffective_PoolSettings(t *testing.T) {
	eff, err := AgentEffective("")
	require.NoError(t, err)
	assert.Nil(t, eff.PoolIdleTimeout)
	assert.Nil(t, eff.PoolMaxIdle)
	assert.Equal(t, 30*time.Minute, eff.PoolIdleTimeoutOrDefault(), "unset means 30m")
	assert.Equal(t, 4, eff.PoolMaxIdleOrDefault(), "unset means 4")

	t.Setenv("UNIFIED_AGENT_POOL_IDLE_TIMEOUT", "0")
	t.Setenv("UNIFIED_AGENT_POOL_MAX_IDLE", "0")
	eff, err = AgentEffective("")
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), eff.PoolIdleTimeoutOrDefault(), "an explicit 0 is kept, not replaced by the default")
	assert.Equal(t, 0, eff.PoolMaxIdleOrDefault())

	t.Setenv("UNIFIED_AGENT_POOL_IDLE_TIMEOUT", "10m")
	t.Setenv("UNIFIED_AGENT_POOL_MAX_IDLE", "2")
	eff, err = AgentEffective("")
	require.NoError(t, err)
	assert.Equal(t, 10*time.Minute, eff.PoolIdleTimeoutOrDefault())
	assert.Equal(t, 2, eff.PoolMaxIdleOrDefault())

	p := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(p, []byte("poolIdleTimeout: 20m\npoolMaxIdle: 8\n"), 0o600))
	eff, err = AgentEffective(p)
	require.NoError(t, err)
	assert.Equal(t, 20*time.Minute, eff.PoolIdleTimeoutOrDefault(), "file beats env")
	assert.Equal(t, 8, eff.PoolMaxIdleOrDefault())

	require.NoError(t, os.WriteFile(p, []byte("poolIdleTimeout: 0s\npoolMaxIdle: 0\n"), 0o600))
	eff, err = AgentEffective(p)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), eff.PoolIdleTimeoutOrDefault(), "an explicit 0 in the file beats the env value")
	assert.Equal(t, 0, eff.PoolMaxIdleOrDefault())
}
