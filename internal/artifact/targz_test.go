package artifact

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// makeTarZstd builds a tar+zstd stream from name->content entries.
func makeTarZstd(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// makeTarZstdWithSymlink builds a tar+zstd stream containing one symlink entry
// (name -> linkname) plus any additional regular files.
func makeTarZstdWithSymlink(t *testing.T, name, linkname string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o777,
		Typeflag: tar.TypeSymlink,
		Linkname: linkname,
	}); err != nil {
		t.Fatal(err)
	}
	for fname, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: fname, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractTarZstd_RoundTrip(t *testing.T) {
	dest := t.TempDir()
	data := makeTarZstd(t, map[string]string{"a.txt": "hello", "sub/b.txt": "world"})
	if err := ExtractTarZstd(bytes.NewReader(data), dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("a.txt = %q, %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(dest, "sub", "b.txt"))
	if err != nil || string(got) != "world" {
		t.Fatalf("sub/b.txt = %q, %v", got, err)
	}
}

func TestExtractTarZstd_RejectsTraversal(t *testing.T) {
	dest := t.TempDir()
	data := makeTarZstd(t, map[string]string{"../escape.txt": "evil"})
	if err := ExtractTarZstd(bytes.NewReader(data), dest); err == nil {
		t.Fatal("expected path-traversal rejection, got nil")
	}
}

func TestExtractTarZstd_RelativeDestDot(t *testing.T) {
	// Regression (#18): extracting into the default dest "." must succeed, not
	// trip the traversal guard.
	dir := t.TempDir()
	t.Chdir(dir)
	data := makeTarZstd(t, map[string]string{"out.txt": "hi", "sub/b.txt": "there"})
	if err := ExtractTarZstd(bytes.NewReader(data), "."); err != nil {
		t.Fatalf("extract into '.': %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "out.txt")); err != nil || string(got) != "hi" {
		t.Fatalf("out.txt = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "sub", "b.txt")); err != nil || string(got) != "there" {
		t.Fatalf("sub/b.txt = %q, %v", got, err)
	}
}

func TestExtractTarZstd_RelativeDest_StillRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	data := makeTarZstd(t, map[string]string{"../escape.txt": "evil"})
	if err := ExtractTarZstd(bytes.NewReader(data), "."); err == nil {
		t.Fatal("expected traversal rejection with dest '.', got nil")
	}
}

func TestExtractTarZstd_SymlinkEntryFailsLoudlyAndCreatesNoSymlink(t *testing.T) {
	dest := t.TempDir()
	data := makeTarZstdWithSymlink(t, "link", "/etc/passwd", map[string]string{"a.txt": "hello"})
	err := ExtractTarZstd(bytes.NewReader(data), dest)
	if err == nil {
		t.Fatal("expected error extracting archive containing a symlink entry, got nil")
	}
	if _, statErr := os.Lstat(filepath.Join(dest, "link")); statErr == nil {
		t.Fatal("extraction must not create a real symlink on disk")
	}
}

func TestExtractTarZstd_HardlinkEntryFailsLoudly(t *testing.T) {
	dest := t.TempDir()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: "a.txt", Mode: 0o600, Size: 5, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "hardlink", Mode: 0o600, Typeflag: tar.TypeLink, Linkname: "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := ExtractTarZstd(bytes.NewReader(buf.Bytes()), dest); err == nil {
		t.Fatal("expected error extracting archive containing a hardlink entry, got nil")
	}
}
