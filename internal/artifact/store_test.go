package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/eirueimi/unified-cd/internal/objectstore"
)

func TestUploadDownload_RoundTrip(t *testing.T) {
	store := objectstore.NewLocalObjectStore(t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("world"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := Upload(ctx, store, "run1", "build", src); err != nil {
		t.Fatalf("upload: %v", err)
	}
	dest := t.TempDir()
	if err := Download(ctx, store, "run1", "build", dest); err != nil {
		t.Fatalf("download: %v", err)
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

func TestUpload_UsesArtifactKeyLayout(t *testing.T) {
	dir := t.TempDir()
	store := objectstore.NewLocalObjectStore(dir)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "x"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Upload(context.Background(), store, "runXYZ", "myart", src); err != nil {
		t.Fatal(err)
	}
	// LocalObjectStore writes files under baseDir/<key>; assert the key path exists.
	if _, err := os.Stat(filepath.Join(dir, "artifacts", "runXYZ", "myart.tar.gz")); err != nil {
		t.Fatalf("expected artifacts/runXYZ/myart.tar.gz: %v", err)
	}
}
