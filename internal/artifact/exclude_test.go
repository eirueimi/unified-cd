package artifact

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

func entriesOf(t *testing.T, dir string) []string {
	t.Helper()
	var got []string
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel != "." {
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	}))
	sort.Strings(got)
	return got
}

func TestWriteTarZstdExcluding_LeavesOutAPathAndEverythingBelowIt(t *testing.T) {
	src := t.TempDir()
	for _, f := range []string{"keep.txt", "sub/keep.txt", "vol/a.txt", "vol/deep/b.txt", "volume-lookalike.txt", "sub/drop.txt"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(src, f)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(src, f), []byte(f), 0o600))
	}

	rc := StreamTarZstdExcluding(src, []string{filepath.Join(src, "vol"), filepath.Join(src, "sub", "drop.txt"), filepath.Join(src, "does-not-exist")})
	defer rc.Close()
	dest := t.TempDir()
	require.NoError(t, ExtractTarZstd(rc, dest))

	require.Equal(t, []string{"keep.txt", "sub", "sub/keep.txt", "volume-lookalike.txt"}, entriesOf(t, dest),
		"the directory and its contents are gone, a name that merely starts the same is kept, and an exclusion that is not there is harmless")
}

func TestWriteTarZstdExcluding_NoExclusionsIsTheOrdinaryArchive(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "d", "f"), []byte("x"), 0o600))

	rc := StreamTarZstdExcluding(src, nil)
	defer rc.Close()
	dest := t.TempDir()
	require.NoError(t, ExtractTarZstd(rc, dest))
	require.Equal(t, []string{"d", "d/f"}, entriesOf(t, dest))
}
