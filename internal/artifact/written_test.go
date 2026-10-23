package artifact

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractTarZstdWritten_ReportsOnlyWhatItWrote(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "old", "deep"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(src, "fresh", "deep"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "top.txt"), []byte("t"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "old", "x.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "fresh", "deep", "y.txt"), []byte("y"), 0o644))
	var buf bytes.Buffer
	require.NoError(t, WriteTarZstdExcluding(&buf, src, nil))

	dest := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dest, "old"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dest, "old", "job.txt"), []byte("j"), 0o644))

	var w Written
	require.NoError(t, ExtractTarZstdWritten(&buf, dest, nil, &w))

	assert.Equal(t, []string{
		filepath.Join(dest, "fresh"), // newly created: covers everything below
		filepath.Join(dest, "old", "deep"),
		filepath.Join(dest, "old", "x.txt"),
		filepath.Join(dest, "top.txt"),
	}, w.Roots())
}

func TestWritten_NilRecordsNothing(t *testing.T) {
	var w *Written
	w.File("x")
	require.NoError(t, w.MkdirAll(filepath.Join(t.TempDir(), "a", "b"), 0o755))
	assert.Nil(t, w.Roots())
}
