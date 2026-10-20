package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const volumesYAML = `volumes:
  - name: unity-main
    mode: exclusive
    path: /var/lib/unified-cd/volumes/unity-main
    size: 200Gi
    storageClassName: gp3
  - name: go-cache
    mode: shared
`

func writeAgentYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "unified-agent.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestLoadAgent_Volumes(t *testing.T) {
	cfg, err := LoadAgent(writeAgentYAML(t, volumesYAML))
	require.NoError(t, err)
	require.Equal(t, []VolumeConfig{
		{Name: "unity-main", Mode: "exclusive", Path: "/var/lib/unified-cd/volumes/unity-main", Size: "200Gi", StorageClassName: "gp3"},
		{Name: "go-cache", Mode: "shared"},
	}, cfg.Volumes)
}

func TestLoadAgent_VolumesRejectsUnknownField(t *testing.T) {
	_, err := LoadAgent(writeAgentYAML(t, "volumes:\n  - name: a\n    mode: shared\n    sizee: 1Gi\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sizee")
}

func TestAgentEffective_VolumesComeFromTheFileOnly(t *testing.T) {
	eff, err := AgentEffective(writeAgentYAML(t, volumesYAML))
	require.NoError(t, err)
	require.Len(t, eff.Volumes, 2)
	assert.Equal(t, "go-cache", eff.Volumes[1].Name)

	// No file, no volumes: there is no env var or flag spelling to fall back on.
	eff, err = AgentEffective("")
	require.NoError(t, err)
	assert.Empty(t, eff.Volumes)
}
