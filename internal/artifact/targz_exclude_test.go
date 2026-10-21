package artifact

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// packEntries writes a tar+zstd archive of the named regular files and, for a
// name ending in "@", a symlink entry of that name.
func packEntries(t *testing.T, names ...string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(enc)
	for _, n := range names {
		if n[len(n)-1] == '@' {
			if err := tw.WriteHeader(&tar.Header{Name: n[:len(n)-1], Typeflag: tar.TypeSymlink, Linkname: "elsewhere"}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o600, Size: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var got []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	return got
}

// An entry at or below an excluded path is never extracted, whatever its type;
// a name that merely starts the same way is not below it.
func TestExtractTarZstdExcluding_SkipsEntriesAtAndBelowExcludedPaths(t *testing.T) {
	dest := t.TempDir()
	err := ExtractTarZstdExcluding(packEntries(t,
		"keep.txt", "vol/big.bin", "vol/deep/obj.o", "vol", "vol2/near.txt", "volume.txt", "link/under@", "link/under/x"),
		dest, []string{filepath.Join(dest, "vol"), filepath.Join(dest, "link", "under")})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	got := listFiles(t, dest)
	want := []string{"keep.txt", "vol2/near.txt", "volume.txt"}
	if len(got) != len(want) {
		t.Fatalf("extracted %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("extracted %v, want %v", got, want)
		}
	}
}

// With nothing excluded it is ExtractTarZstd, which still refuses a symlink.
func TestExtractTarZstdExcluding_NothingExcludedChangesNothing(t *testing.T) {
	dest := t.TempDir()
	if err := ExtractTarZstdExcluding(packEntries(t, "a/b.txt", "c.txt"), dest, nil); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got := listFiles(t, dest); len(got) != 2 {
		t.Fatalf("extracted %v", got)
	}
	if err := ExtractTarZstdExcluding(packEntries(t, "l@"), t.TempDir(), nil); err == nil {
		t.Fatal("a symlink entry must still be refused when it is not in an excluded path")
	}
}
