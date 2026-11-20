package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControllerConfig_RedirectNeedsS3(t *testing.T) {
	err := config.CheckArtifactDownload("redirect", false)
	require.EqualError(t, err, "objectStore.artifactDownload: redirect requires an S3-compatible object store")
	require.NoError(t, config.CheckArtifactDownload("redirect", true))
	require.NoError(t, config.CheckArtifactDownload("proxy", false))
	require.NoError(t, config.CheckArtifactDownload("", false))
	require.EqualError(t, config.CheckArtifactDownload("bogus", true),
		`objectStore.artifactDownload: must be "proxy" or "redirect", got "bogus"`)
}

func TestControllerEffective_ArtifactDownload(t *testing.T) {
	t.Setenv("UNIFIED_DEV_MODE", "true")
	t.Setenv("UNIFIED_ARTIFACT_DOWNLOAD", "")
	eff, err := config.ControllerEffective("")
	require.NoError(t, err)
	assert.Equal(t, "proxy", eff.ArtifactDownloadMode())

	t.Setenv("UNIFIED_ARTIFACT_DOWNLOAD", "redirect")
	eff, err = config.ControllerEffective("")
	require.NoError(t, err)
	assert.Equal(t, "redirect", eff.ArtifactDownloadMode())

	// The config file wins over the environment.
	path := filepath.Join(t.TempDir(), "c.yaml")
	require.NoError(t, os.WriteFile(path, []byte("objectStore:\n  artifactDownload: proxy\n"), 0o600))
	eff, err = config.ControllerEffective(path)
	require.NoError(t, err)
	assert.Equal(t, "proxy", eff.ArtifactDownloadMode())

	require.NoError(t, os.WriteFile(path, []byte("objectStore:\n  artifactDownload: nope\n"), 0o600))
	_, err = config.ControllerEffective(path)
	require.EqualError(t, err, `objectStore.artifactDownload: must be "proxy" or "redirect", got "nope"`)

	t.Setenv("UNIFIED_ARTIFACT_DOWNLOAD", "nope")
	_, err = config.ControllerEffective("")
	require.ErrorContains(t, err, `must be "proxy" or "redirect", got "nope"`)
}
