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
