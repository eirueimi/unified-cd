package objectstore

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	assert.Error(t, err)
}

func TestLocalObjectStore_GetNotFound(t *testing.T) {
	store := NewLocalObjectStore(t.TempDir())
	_, err := store.Get(context.Background(), "nonexistent")
	assert.Error(t, err)
}
