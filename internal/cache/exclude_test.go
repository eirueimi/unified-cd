package cache

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/objectstore"
)

func treeEntries(t *testing.T, dir string) []string {
	t.Helper()
	var got []string
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if rel, _ := filepath.Rel(dir, p); err == nil && rel != "." {
			got = append(got, filepath.ToSlash(rel))
		}
		return err
	}))
	sort.Strings(got)
	return got
}

// An archive saved before a directory became a volume's mount point holds files
// there. Restoring it must put the rest back and leave the volume alone.
func TestRestoreExcluding_SkipsEntriesAtAndBelowAnExcludedPath(t *testing.T) {
	ctx := context.Background()
	store := objectstore.NewLocalObjectStore(t.TempDir())
	src := t.TempDir()
	for _, f := range []string{"keep.txt", "sub/keep.txt", "vol/a.txt", "vol/deep/b.txt", "volume-lookalike.txt"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(src, f)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(src, f), []byte(f), 0o600))
	}
	require.NoError(t, Save(ctx, store, "job", src, "k", 7))

	dest := t.TempDir()
	hit, err := RestoreExcluding(ctx, store, "job", dest, "k", nil, []string{filepath.Join(dest, "vol"), filepath.Join(dest, "absent")})
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, []string{"keep.txt", "sub", "sub/keep.txt", "volume-lookalike.txt"}, treeEntries(t, dest))

	// The fallback key path honours it too.
	dest2 := t.TempDir()
	hit, err = RestoreExcluding(ctx, store, "job", dest2, "k-no-exact", []string{"k"}, []string{filepath.Join(dest2, "vol")})
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, []string{"keep.txt", "sub", "sub/keep.txt", "volume-lookalike.txt"}, treeEntries(t, dest2))
}
