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

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
}

func tarEntries(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	dec, err := zstd.NewReader(bytes.NewReader(archive))
	require.NoError(t, err)
	defer dec.Close()
	tr := tar.NewReader(dec)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return got
		}
		require.NoError(t, err)
		b, err := io.ReadAll(tr)
		require.NoError(t, err)
		got[h.Name] = string(b)
	}
}

func buildArchive(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	require.NoError(t, err)
	tw := tar.NewWriter(enc)
	for name, body := range entries {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, enc.Close())
	return buf.Bytes()
}

// A symlink in the workspace that leads out of it must not let an archive
// read what is on the other side (the sidecar's credential mount).
func TestWriteTarZstdIn_DoesNotFollowSymlinkOutOfRoot(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "s3-credentials"), []byte("TOPSECRET"), 0o600))
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "ok.txt"), []byte("fine"), 0o600))
	symlinkOrSkip(t, outside, filepath.Join(root, "l"))
	symlinkOrSkip(t, filepath.Join(outside, "s3-credentials"), filepath.Join(root, "f"))

	t.Run("path through a directory link is refused", func(t *testing.T) {
		var buf bytes.Buffer
		err := WriteTarZstdIn(&buf, root, filepath.Join(root, "l", "s3-credentials"), nil)
		require.Error(t, err)
		require.NotContains(t, buf.String(), "TOPSECRET")
		for _, c := range tarEntries(t, buf.Bytes()) {
			require.NotContains(t, c, "TOPSECRET")
		}
	})
	t.Run("a link met while walking is archived as a link, not read", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, WriteTarZstdIn(&buf, root, root, nil))
		got := tarEntries(t, buf.Bytes())
		require.Equal(t, "fine", got["ok.txt"])
		require.Contains(t, got, "f")
		require.Contains(t, got, "l")
		for name, c := range got {
			require.NotContains(t, c, "TOPSECRET", name)
		}
	})
	t.Run("a path outside the root is refused", func(t *testing.T) {
		err := WriteTarZstdIn(io.Discard, root, outside, nil)
		require.Error(t, err)
	})
}

// A symlink that stays inside the root keeps working: the path may go through
// it, and a link met while walking is archived as a link entry.
func TestWriteTarZstdIn_InRootSymlinksStillWork(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "real"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "real", "a.txt"), []byte("A"), 0o600))
	symlinkOrSkip(t, "real", filepath.Join(root, "alias"))

	var buf bytes.Buffer
	require.NoError(t, WriteTarZstdIn(&buf, root, filepath.Join(root, "alias", "a.txt"), nil))
	require.Equal(t, "A", tarEntries(t, buf.Bytes())["a.txt"])

	buf.Reset()
	require.NoError(t, WriteTarZstdIn(&buf, root, root, nil))
	got := tarEntries(t, buf.Bytes())
	require.Equal(t, "A", got["real/a.txt"])
	require.Contains(t, got, "alias")
	require.Empty(t, got["alias"])
}

func TestWriteTarZstdIn_ExcludeStillApplies(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "vol"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "vol", "big"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "keep"), []byte("k"), 0o600))
	var buf bytes.Buffer
	require.NoError(t, WriteTarZstdIn(&buf, root, root, []string{filepath.Join(root, "vol")}))
	got := tarEntries(t, buf.Bytes())
	require.Contains(t, got, "keep")
	require.NotContains(t, got, "vol")
	require.NotContains(t, got, "vol/big")
}

func TestExtractTarZstdIn_RefusesToWriteThroughSymlinkOutOfRoot(t *testing.T) {
	outside := t.TempDir()
	root := t.TempDir()
	symlinkOrSkip(t, outside, filepath.Join(root, "l"))
	archive := buildArchive(t, map[string]string{"x.txt": "PWNED"})

	t.Run("dest whose parent is a link out", func(t *testing.T) {
		err := ExtractTarZstdIn(bytes.NewReader(archive), root, filepath.Join(root, "l", "sub"), nil, true)
		require.Error(t, err)
		require.NoDirExists(t, filepath.Join(outside, "sub"))
	})
	t.Run("dest that is a link out", func(t *testing.T) {
		err := ExtractTarZstdIn(bytes.NewReader(archive), root, filepath.Join(root, "l"), nil, true)
		require.Error(t, err)
		require.NoFileExists(t, filepath.Join(outside, "x.txt"))
	})
	t.Run("entry whose directory is a link out", func(t *testing.T) {
		err := ExtractTarZstdIn(bytes.NewReader(buildArchive(t, map[string]string{"l/x.txt": "PWNED"})), root, root, nil, true)
		require.Error(t, err)
		require.NoFileExists(t, filepath.Join(outside, "x.txt"))
	})
	t.Run("entry that is a link to a file outside", func(t *testing.T) {
		target := filepath.Join(outside, "ca.pem")
		require.NoError(t, os.WriteFile(target, []byte("ORIGINAL"), 0o600))
		symlinkOrSkip(t, target, filepath.Join(root, "ca-link"))
		err := ExtractTarZstdIn(bytes.NewReader(buildArchive(t, map[string]string{"ca-link": "PWNED"})), root, root, nil, true)
		require.Error(t, err)
		b, _ := os.ReadFile(target)
		require.Equal(t, "ORIGINAL", string(b))
	})
	t.Run("dest outside the root", func(t *testing.T) {
		require.Error(t, ExtractTarZstdIn(bytes.NewReader(archive), root, outside, nil, true))
		require.NoFileExists(t, filepath.Join(outside, "x.txt"))
	})
}

func TestExtractTarZstdIn_InRootSymlinkStillWorks(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "real"), 0o755))
	symlinkOrSkip(t, "real", filepath.Join(root, "alias"))
	require.NoError(t, ExtractTarZstdIn(bytes.NewReader(buildArchive(t, map[string]string{"x.txt": "ok"})), root, filepath.Join(root, "alias"), nil, true))
	b, err := os.ReadFile(filepath.Join(root, "real", "x.txt"))
	require.NoError(t, err)
	require.Equal(t, "ok", string(b))
}

func TestExtractTarZstdIn_RoundTripAndExclude(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "vol"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("A"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "vol", "b.txt"), []byte("B"), 0o600))
	var buf bytes.Buffer
	require.NoError(t, WriteTarZstdIn(&buf, root, src, nil))
	dest := filepath.Join(root, "dest")
	require.NoError(t, ExtractTarZstdIn(&buf, root, dest, []string{filepath.Join(dest, "vol")}, true))
	b, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "A", string(b))
	require.NoFileExists(t, filepath.Join(dest, "vol", "b.txt"))
}

func TestExtractTarZstdInCounting_CountsSkippedLinks(t *testing.T) {
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	require.NoError(t, err)
	tw := tar.NewWriter(enc)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "a.txt", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg}))
	_, err = tw.Write([]byte("hi"))
	require.NoError(t, err)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "sym", Typeflag: tar.TypeSymlink, Linkname: "a.txt"}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "a.txt"}))
	require.NoError(t, tw.Close())
	require.NoError(t, enc.Close())
	archive := buf.Bytes()

	root := t.TempDir()
	n, err := ExtractTarZstdInCounting(bytes.NewReader(archive), root, root, nil, false)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.FileExists(t, filepath.Join(root, "a.txt"))
	_, err = os.Lstat(filepath.Join(root, "sym"))
	require.True(t, os.IsNotExist(err))

	n, err = ExtractTarZstdInCounting(bytes.NewReader(archive), t.TempDir(), "", nil, true)
	_ = n
	require.Error(t, err)
}
