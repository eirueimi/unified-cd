package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig_ReadsYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server: https://example.com\ntoken: t1\n"), 0o600))
	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "https://example.com", cfg.Server)
	assert.Equal(t, "t1", cfg.Token)
}

func TestLoadConfig_MissingFileReturnsEmpty(t *testing.T) {
	cfg, err := LoadConfig("/no/such/file.yaml")
	require.NoError(t, err)
	assert.Empty(t, cfg.Server)
}
