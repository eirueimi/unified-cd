package objectstore

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Compile-time contract check: every ObjectStore implementation must satisfy
// the eager-ErrNotFound contract documented on ObjectStore.Get and
// ErrNotFound. This file exercises that contract against LocalObjectStore,
// which stands in as the "fake" ObjectStore for tests across the repo
// (internal/cache, internal/agent, internal/controller, ...). S3ObjectStore
// implements the identical contract (see s3.go Get's doc comment) but is not
// covered by a unit test here since it requires a live S3-compatible server;
// exercising it is left to the //go:build k8s integration tests, which run
// cache/artifact flows against a real S3-compatible bucket end-to-end.
var _ ObjectStore = (*LocalObjectStore)(nil)

func TestLocalObjectStore_PutAndGet(t *testing.T) {
	store := NewLocalObjectStore(t.TempDir())
	ctx := context.Background()

	content := "hello world\nline 2\n"
	require.NoError(t, store.Put(ctx, "runs/abc/logs.ndjson", strings.NewReader(content), int64(len(content))))

	rc, err := store.Get(ctx, "runs/abc/logs.ndjson")
	require.NoError(t, err)
	defer rc.Close()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content, string(got))
}

func TestLocalObjectStore_Delete(t *testing.T) {
	store := NewLocalObjectStore(t.TempDir())
	ctx := context.Background()

	_ = store.Put(ctx, "key", strings.NewReader("x"), 1)
	require.NoError(t, store.Delete(ctx, "key"))

	_, err := store.Get(ctx, "key")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound, "Get on a deleted key must return the shared ErrNotFound sentinel, eagerly (not on first Read)")
}

func TestLocalObjectStore_GetNotFound(t *testing.T) {
	store := NewLocalObjectStore(t.TempDir())
	_, err := store.Get(context.Background(), "nonexistent")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound, "Get on a missing key must return the shared ErrNotFound sentinel, eagerly (not on first Read)")
}

type failingReader struct {
	data string
	sent bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.sent {
		f.sent = true
		return copy(p, f.data), nil
	}
	return 0, io.ErrUnexpectedEOF
}

// A Put whose body fails halfway (an archiver that hit a permission error)
// must not leave a truncated object under the key, and must not destroy the
// object that was there before.
func TestLocalObjectStore_FailedPutLeavesNoPartialObject(t *testing.T) {
	store := NewLocalObjectStore(t.TempDir())
	ctx := context.Background()

	require.Error(t, store.Put(ctx, "caches/a/b", &failingReader{data: "partial"}, -1))
	_, err := store.Get(ctx, "caches/a/b")
	require.ErrorIs(t, err, ErrNotFound)
	keys, err := store.List(ctx, "")
	require.NoError(t, err)
	assert.Empty(t, keys, "no partial object or temp file is left")

	require.NoError(t, store.Put(ctx, "caches/a/b", strings.NewReader("good"), 4))
	require.Error(t, store.Put(ctx, "caches/a/b", &failingReader{data: "partial"}, -1))
	rc, err := store.Get(ctx, "caches/a/b")
	require.NoError(t, err)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	assert.Equal(t, "good", string(got), "a failed overwrite keeps the previous object")
}
