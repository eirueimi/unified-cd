package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const volumesRemovedMessage = `agent config: "volumes" is no longer supported; declare volumes on the controller with kind: AgentVolume (see docs/operator-manual/migrations/2026-11-18-controller-declared-volumes.md)`

func writeAgentYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "unified-agent.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestConfigRejectsVolumesKey(t *testing.T) {
	for name, body := range map[string]string{
		"entries":    "volumes:\n  - name: a\n    mode: shared\n    size: 1Gi\n",
		"empty list": "volumes: []\n",
		"null":       "volumes:\n",
		"with other": "server: http://localhost:8080\nvolumes:\n  - name: a\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := writeAgentYAML(t, body)
			_, err := LoadAgent(p)
			require.EqualError(t, err, volumesRemovedMessage)
			_, err = AgentEffective(p)
			require.EqualError(t, err, volumesRemovedMessage)
		})
	}
}

func TestLoadAgent_NoVolumesKeyStillLoads(t *testing.T) {
	cfg, err := LoadAgent(writeAgentYAML(t, "server: http://localhost:8080\n"))
	require.NoError(t, err)
	require.Equal(t, "http://localhost:8080", cfg.Server)
}
