package artifact

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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

// os.Create on an existing file keeps its owner, so the Kubernetes sidecar
// leaves a pre-existing file's ownership alone; the host must not re-own it.
func TestExtractTarZstdWritten_DoesNotRecordOverwrittenFiles(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "new.txt"), []byte("n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "existing.txt"), []byte("from artifact"), 0o644))
	var buf bytes.Buffer
	require.NoError(t, WriteTarZstdExcluding(&buf, src, nil))

	dest := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dest, "existing.txt"), []byte("job's own"), 0o644))

	var w Written
	require.NoError(t, ExtractTarZstdWritten(&buf, dest, nil, &w))

	assert.Equal(t, []string{filepath.Join(dest, "new.txt")}, w.Roots(), "the overwritten file keeps its owner")
	got, err := os.ReadFile(filepath.Join(dest, "existing.txt"))
	require.NoError(t, err)
	assert.Equal(t, "from artifact", string(got), "it is still overwritten")
}

// An Lstat failure other than "does not exist" (an unreadable parent) must be
// returned, not read as "the directory is missing".
func TestWritten_MkdirAllReturnsNonNotExistLstatErrors(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("needs a directory the current user cannot traverse")
	}
	base := t.TempDir()
	locked := filepath.Join(base, "locked")
	require.NoError(t, os.Mkdir(locked, 0o755))
	require.NoError(t, os.Chmod(locked, 0))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	var w Written
	err := w.MkdirAll(filepath.Join(locked, "a", "b"), 0o755)
	require.Error(t, err)
	assert.False(t, errors.Is(err, fs.ErrNotExist))
	assert.Empty(t, w.Roots())
}

// The injected Lstat makes the same check testable as root and on Windows.
func TestWritten_MkdirAllLstatErrorIsNotTreatedAsMissing(t *testing.T) {
	old := lstat
	lstat = func(string) (os.FileInfo, error) { return nil, fs.ErrPermission }
	t.Cleanup(func() { lstat = old })

	var w Written
	err := w.MkdirAll(filepath.Join(t.TempDir(), "a"), 0o755)
	require.ErrorIs(t, err, fs.ErrPermission)
	assert.Empty(t, w.newDirs, "nothing was created or recorded")
}
