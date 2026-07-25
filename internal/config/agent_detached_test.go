package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadAgent_MaxDetachedConcurrent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.yaml")
	require.NoError(t, os.WriteFile(p, []byte("maxDetachedConcurrent: 8\n"), 0o600))

	cfg, err := LoadAgent(p)
	require.NoError(t, err)
	assert.Equal(t, 8, cfg.MaxDetachedConcurrent)
}

func TestAgentEffective_MaxDetachedFromEnv(t *testing.T) {
	t.Setenv("UNIFIED_AGENT_MAX_DETACHED", "5")
	eff, err := AgentEffective("")
	require.NoError(t, err)
	assert.Equal(t, 5, eff.MaxDetachedConcurrent)
}
