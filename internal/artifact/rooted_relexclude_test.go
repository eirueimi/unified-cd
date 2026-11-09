package artifact

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An exclusion given relative (a relative --workspace-dir) must still match, as
// it does for the unconfined archiver.
func TestWriteTarZstdIn_RelativeExclusionStillMatches(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	require.NoError(t, os.MkdirAll(filepath.Join("ws", "vol"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join("ws", "vol", "inside"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join("ws", "keep"), []byte("y"), 0o644))

	var buf bytes.Buffer
	require.NoError(t, WriteTarZstdIn(&buf, "ws", ".", []string{filepath.Join("ws", "vol")}))
	names := map[string]bool{}
	for n := range tarEntries(t, buf.Bytes()) {
		names[n] = true
	}
	assert.True(t, names["keep"])
	assert.False(t, names["vol"], "the excluded directory must not be archived")
	assert.False(t, names["vol/inside"])
}
