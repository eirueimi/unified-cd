package artifact

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

// linkTree builds a node_modules-style tree: a regular file, a symlink to a
// non-empty file, a symlink to a directory and a dangling symlink.
func linkTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "pkg", "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pkg", "bin", "cli.js"), []byte("console.log(1)"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".bin"), 0o755))
	symlinkOrSkip(t, filepath.Join("..", "pkg", "bin", "cli.js"), filepath.Join(dir, ".bin", "cli"))
	symlinkOrSkip(t, "pkg", filepath.Join(dir, "pkg-link"))
	symlinkOrSkip(t, "does-not-exist", filepath.Join(dir, "dangling"))
	return dir
}

func linkTypes(t *testing.T, archive []byte) map[string]tar.Header {
	t.Helper()
	dec, err := zstd.NewReader(bytes.NewReader(archive))
	require.NoError(t, err)
	defer dec.Close()
	tr := tar.NewReader(dec)
	got := map[string]tar.Header{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return got
		}
		require.NoError(t, err)
		got[h.Name] = *h
	}
}

func TestWriteTarZstdExcluding_StoresSymlinksAsLinks(t *testing.T) {
	dir := linkTree(t)

	var buf bytes.Buffer
	require.NoError(t, WriteTarZstdExcluding(&buf, dir, nil))

	hdrs := linkTypes(t, buf.Bytes())
	require.Equal(t, byte(tar.TypeSymlink), hdrs[".bin/cli"].Typeflag)
	require.Equal(t, filepath.Join("..", "pkg", "bin", "cli.js"), filepath.FromSlash(hdrs[".bin/cli"].Linkname))
	require.Equal(t, byte(tar.TypeSymlink), hdrs["pkg-link"].Typeflag, "a link to a directory is not followed")
	require.Equal(t, "pkg", hdrs["pkg-link"].Linkname)
	require.Equal(t, byte(tar.TypeSymlink), hdrs["dangling"].Typeflag, "a dangling link is archived, not an error")
	require.Equal(t, byte(tar.TypeReg), hdrs["pkg/bin/cli.js"].Typeflag)
	_, followed := hdrs["pkg-link/bin/cli.js"]
	require.False(t, followed, "nothing below a linked directory is archived")
}

func TestWriteTarZstdExcluding_SymlinkAsSingleFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "real.txt"), []byte("hello"), 0o644))
	symlinkOrSkip(t, "real.txt", filepath.Join(dir, "alias.txt"))

	var buf bytes.Buffer
	require.NoError(t, WriteTarZstdExcluding(&buf, filepath.Join(dir, "alias.txt"), nil))
	hdrs := linkTypes(t, buf.Bytes())
	require.Equal(t, byte(tar.TypeSymlink), hdrs["alias.txt"].Typeflag)
}

func TestWriteTarZstdExcluding_ExcludeStillApplies(t *testing.T) {
	dir := linkTree(t)
	var buf bytes.Buffer
	require.NoError(t, WriteTarZstdExcluding(&buf, dir, []string{filepath.Join(dir, "pkg"), filepath.Join(dir, "dangling")}))
	hdrs := linkTypes(t, buf.Bytes())
	_, hasPkg := hdrs["pkg/bin/cli.js"]
	_, hasDangling := hdrs["dangling"]
	require.False(t, hasPkg)
	require.False(t, hasDangling)
	require.Contains(t, hdrs, "pkg-link")
}

func TestExtractTarZstdDir_CacheSkipsLinksAndCountsThem(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteTarZstdExcluding(&buf, linkTree(t), nil))

	dest := filepath.Join(t.TempDir(), "restored")
	var w Written
	skipped, err := ExtractTarZstdDir(&buf, dest, nil, false, &w)
	require.NoError(t, err)
	require.Equal(t, 3, skipped)

	b, err := os.ReadFile(filepath.Join(dest, "pkg", "bin", "cli.js"))
	require.NoError(t, err)
	require.Equal(t, "console.log(1)", string(b))
	_, err = os.Lstat(filepath.Join(dest, ".bin", "cli"))
	require.ErrorIs(t, err, os.ErrNotExist, "a skipped link leaves nothing behind")
	_, err = os.Lstat(filepath.Join(dest, "dangling"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, []string{dest}, w.Roots(), "the new destination directory is recorded for re-owning")
}

func TestExtractTarZstdDir_ArtifactRefusesLinks(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteTarZstdExcluding(&buf, linkTree(t), nil))

	_, err := ExtractTarZstdDir(&buf, t.TempDir(), nil, true, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported symlink entry")
	require.Contains(t, err.Error(), "in artifact archive")
}

func TestExtractTarZstdDir_RecordsOnlyCreatedFiles(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a"), []byte("new"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "b"), []byte("new"), 0o644))
	var buf bytes.Buffer
	require.NoError(t, WriteTarZstdExcluding(&buf, src, nil))

	dest := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dest, "a"), []byte("old"), 0o644))
	var w Written
	_, err := ExtractTarZstdDir(&buf, dest, nil, false, &w)
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(dest, "b")}, w.Roots())
}

func TestExtractTarZstdDir_ExcludeSkipsMountPoint(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "vol"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "vol", "f"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "keep"), []byte("x"), 0o644))
	var buf bytes.Buffer
	require.NoError(t, WriteTarZstdExcluding(&buf, src, nil))

	dest := t.TempDir()
	_, err := ExtractTarZstdDir(&buf, dest, []string{filepath.Join(dest, "vol")}, false, nil)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(dest, "keep"))
	_, err = os.Lstat(filepath.Join(dest, "vol"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestExtractTarZstdDir_NeverWritesThroughALinkLeavingTheDestination(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "link"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "link", "x"), []byte("pwned"), 0o644))
	var archive bytes.Buffer
	require.NoError(t, WriteTarZstdExcluding(&archive, src, nil))

	for _, refuse := range []bool{true, false} {
		outside := t.TempDir()
		dest := t.TempDir()
		symlinkOrSkip(t, outside, filepath.Join(dest, "link"))

		_, err := ExtractTarZstdDir(bytes.NewReader(archive.Bytes()), dest, nil, refuse, nil)
		require.Error(t, err)
		entries, rerr := os.ReadDir(outside)
		require.NoError(t, rerr)
		require.Empty(t, entries, "nothing may be written outside the destination")
	}
}
