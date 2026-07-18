package artifact

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	if _, err := ArtifactKey("victim-run", "../victim-run/x"); err == nil {
		t.Fatalf("expected ArtifactKey to reject name containing \"..\"")
	}
	if _, err := ArtifactKey("run1", "a/../../b"); err == nil {
		t.Fatalf("expected ArtifactKey to reject name containing \"..\"")
	}
	if _, err := ArtifactKey("run1", "a/b"); err == nil {
		t.Fatalf("expected ArtifactKey to reject name containing \"/\"")
	}
}

func TestArtifactKey_RejectsTraversalInRunID(t *testing.T) {
	if _, err := ArtifactKey("../other", "name"); err == nil {
		t.Fatalf("expected ArtifactKey to reject runID containing \"..\"")
	}
}

func TestArtifactKey_ValidNamesUnchanged(t *testing.T) {
	key, err := ArtifactKey("runXYZ", "myart")
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

func TestStreamTarZstd_RoundTripEqualsBuffered(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "sub"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("world"), 0o600))

	// Stream and extract; the tree must round-trip.
	rc := StreamTarZstd(src)
	defer rc.Close()
	dest := t.TempDir()
	require.NoError(t, ExtractTarZstd(rc, dest))
	got, err := os.ReadFile(filepath.Join(dest, "sub", "b.txt"))
	require.NoError(t, err)
	assert.Equal(t, "world", string(got))
	gotA, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(gotA))
}

func TestStreamTarZstd_SingleFile(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "out.txt"), []byte("data"), 0o600))

	rc := StreamTarZstd(filepath.Join(src, "out.txt"))
	defer rc.Close()
	dest := t.TempDir()
	require.NoError(t, ExtractTarZstd(rc, dest))
	got, err := os.ReadFile(filepath.Join(dest, "out.txt"))
	require.NoError(t, err)
	assert.Equal(t, "data", string(got))
}

func TestStreamTarZstd_ProducerErrorPropagates(t *testing.T) {
	// A path that does not exist makes WriteTarZstd's WalkDir fail; the error
	// must surface to the consumer as a read error, and the read must not hang.
	rc := StreamTarZstd(filepath.Join(t.TempDir(), "does-not-exist"))
	defer rc.Close()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(rc); done <- err }()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("read hung waiting for producer error")
	}
}

func TestStreamTarZstd_CloseUnblocksProducer(t *testing.T) {
	// A source with many files keeps the producer writing; Close must unblock
	// it (io.Pipe is unbuffered) and a subsequent Read returns ErrClosedPipe.
	src := t.TempDir()
	for i := 0; i < 200; i++ {
		require.NoError(t, os.WriteFile(
			filepath.Join(src, "f"+strconv.Itoa(i)+".txt"),
			bytes.Repeat([]byte("x"), 1024), 0o600))
	}
	rc := StreamTarZstd(src)
	buf := make([]byte, 16)
	_, _ = rc.Read(buf) // consume one chunk; producer is now mid-archive
	require.NoError(t, rc.Close())
	_, err := rc.Read(buf)
	assert.ErrorIs(t, err, io.ErrClosedPipe)
}

// sizeSpyStore records the size argument passed to Put and delegates to an
// embedded real store so Get/List/Delete still work.
type sizeSpyStore struct {
	objectstore.ObjectStore
	lastSize int64
}

func (s *sizeSpyStore) Put(ctx context.Context, key string, content io.Reader, size int64) error {
	s.lastSize = size
	return s.ObjectStore.Put(ctx, key, content, size)
}

func TestUpload_StreamsWithUnknownSize(t *testing.T) {
	// Upload must stream: it passes size == -1 to Put (not the buffered length).
	spy := &sizeSpyStore{ObjectStore: objectstore.NewLocalObjectStore(t.TempDir())}
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o600))

	require.NoError(t, Upload(context.Background(), spy, "run1", "art1", src))
	assert.Equal(t, int64(-1), spy.lastSize, "Upload must stream with unknown size (-1)")
}
