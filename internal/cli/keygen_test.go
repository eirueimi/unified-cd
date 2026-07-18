package cli

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeygen_PrintsHexKeyToStdout(t *testing.T) {
	cmd := newKeygenCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{})
	require.NoError(t, cmd.Execute())

	got := strings.TrimSpace(out.String())
	assert.Len(t, got, 64)
	_, err := hex.DecodeString(got)
	require.NoError(t, err)
}

func TestKeygen_WritesFileWith0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kek")
	cmd := newKeygenCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"--out", path})
	require.NoError(t, cmd.Execute())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Len(t, strings.TrimSpace(string(data)), 64)

	// Shell redirection would create this under the caller's umask, commonly
	// 0644. Writing the file ourselves is the point of --out.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestKeygen_RefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kek")
	require.NoError(t, os.WriteFile(path, []byte("existing"), 0o600))

	cmd := newKeygenCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"--out", path})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exists")

	data, _ := os.ReadFile(path)
	assert.Equal(t, "existing", string(data), "an existing key must never be clobbered")
}

func TestKeygen_GeneratesDifferentKeys(t *testing.T) {
	run := func() string {
		cmd := newKeygenCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{})
		require.NoError(t, cmd.Execute())
		return strings.TrimSpace(out.String())
	}
	assert.NotEqual(t, run(), run())
}
