package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverAgentCredentialFile(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		got, err := discoverAgentCredentialFile(t.TempDir())
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("one ID-scoped credential", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "agent-a", "credential.json")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))

		got, err := discoverAgentCredentialFile(root)
		require.NoError(t, err)
		assert.Equal(t, path, got)
	})

	t.Run("legacy shared credential is ignored", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "credential.json"), []byte("{}"), 0o600))

		got, err := discoverAgentCredentialFile(root)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("multiple credentials are ambiguous", func(t *testing.T) {
		root := t.TempDir()
		for _, id := range []string{"agent-a", "agent-b"} {
			path := filepath.Join(root, id, "credential.json")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
		}

		_, err := discoverAgentCredentialFile(root)
		require.EqualError(t, err, "multiple default agent credential files found; set --id or --credential-file")
	})
}
