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

func TestUpload_NameWithParentTraversalCannotEscapeRunNamespace(t *testing.T) {
	dir := t.TempDir()
	store := objectstore.NewLocalObjectStore(dir)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "x"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := Upload(context.Background(), store, "victim-run", "../victim-run/x", src)
	if err == nil {
		t.Fatalf("expected Upload to reject traversal name, got nil error")
	}

	// Ensure nothing was written outside the artifacts/ prefix, and nothing
	// landed in another run's namespace either.
	if _, statErr := os.Stat(filepath.Join(dir, "artifacts", "victim-run")); statErr == nil {
		t.Fatalf("traversal name must not create victim-run artifact directory")
	}
}

func TestUpload_NameWithDoubleTraversalCannotEscapeArtifactsPrefix(t *testing.T) {
	dir := t.TempDir()
	store := objectstore.NewLocalObjectStore(dir)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "x"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := Upload(context.Background(), store, "run1", "a/../../b", src)
	if err == nil {
		t.Fatalf("expected Upload to reject traversal name, got nil error")
	}

	// Nothing should have escaped dir/artifacts/.
	if _, statErr := os.Stat(filepath.Join(dir, "b.tar.gz")); statErr == nil {
		t.Fatalf("traversal name must not escape the artifacts/ prefix")
	}
}

func TestArtifactKey_RejectsTraversalInName(t *testing.T) {
	if _, err := artifactKey("victim-run", "../victim-run/x"); err == nil {
		t.Fatalf("expected artifactKey to reject name containing \"..\"")
	}
	if _, err := artifactKey("run1", "a/../../b"); err == nil {
		t.Fatalf("expected artifactKey to reject name containing \"..\"")
	}
	if _, err := artifactKey("run1", "a/b"); err == nil {
		t.Fatalf("expected artifactKey to reject name containing \"/\"")
	}
}

func TestArtifactKey_RejectsTraversalInRunID(t *testing.T) {
	if _, err := artifactKey("../other", "name"); err == nil {
		t.Fatalf("expected artifactKey to reject runID containing \"..\"")
	}
}

func TestArtifactKey_ValidNamesUnchanged(t *testing.T) {
	key, err := artifactKey("runXYZ", "myart")
	if err != nil {
		t.Fatalf("unexpected error for valid name: %v", err)
	}
	if key != "artifacts/runXYZ/myart.tar.gz" {
		t.Fatalf("expected byte-identical key for valid names, got %q", key)
	}
}

func TestUploadDownload_SingleFile(t *testing.T) {
	store := objectstore.NewLocalObjectStore(t.TempDir())
	src := t.TempDir()
	f := filepath.Join(src, "out.txt")
	if err := os.WriteFile(f, []byte("single"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := Upload(ctx, store, "r1", "a", f); err != nil { // upload a single FILE path
		t.Fatalf("upload single file: %v", err)
	}
	dest := t.TempDir()
	if err := Download(ctx, store, "r1", "a", dest); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "out.txt"))
	if err != nil || string(got) != "single" {
		t.Fatalf("dest/out.txt = %q, %v", got, err)
	}
}
