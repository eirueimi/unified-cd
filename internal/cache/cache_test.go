package cache_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unified-cd/unified-cd/internal/cache"
	"github.com/unified-cd/unified-cd/internal/objectstore"
)

func newStore(t *testing.T) objectstore.ObjectStore {
	t.Helper()
	return objectstore.NewLocalObjectStore(t.TempDir())
}

func makeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	return dir
}

func TestCache_SaveAndRestore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	src := makeDir(t, map[string]string{"a.txt": "hello", "b.txt": "world"})
	dest := t.TempDir()

	require.NoError(t, cache.Save(ctx, store, src, "mykey", 7))

	hit, err := cache.Restore(ctx, store, dest, "mykey", nil)
	require.NoError(t, err)
	assert.True(t, hit)

	got, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
}

func TestCache_RestoreKeys_FallbackOnMiss(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	src := makeDir(t, map[string]string{"pkg.json": "v1"})

	// Save with key "npm-abc123"
	require.NoError(t, cache.Save(ctx, store, src, "npm-abc123", 7))

	// Restore with exact miss but prefix match
	dest := t.TempDir()
	hit, err := cache.Restore(ctx, store, dest, "npm-xyz999", []string{"npm-"})
	require.NoError(t, err)
	assert.True(t, hit)

	got, err := os.ReadFile(filepath.Join(dest, "pkg.json"))
	require.NoError(t, err)
	assert.Equal(t, "v1", string(got))
}

func TestCache_Restore_MissNoFallback(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	dest := t.TempDir()

	hit, err := cache.Restore(ctx, store, dest, "missing-key", nil)
	assert.False(t, hit)
	assert.ErrorIs(t, err, cache.ErrCacheMiss)
}

func TestCache_DeleteExpired_RemovesOldKeepsNew(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	src := makeDir(t, map[string]string{"f.txt": "data"})

	require.NoError(t, cache.Save(ctx, store, src, "old-key", 1))
	require.NoError(t, cache.Save(ctx, store, src, "new-key", 30))

	// Delete entries expiring before "now + 2 days" (removes old-key, keeps new-key)
	n, err := cache.DeleteExpired(ctx, store, time.Now().Add(2*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// old-key should be gone
	dest := t.TempDir()
	hit, _ := cache.Restore(ctx, store, dest, "old-key", nil)
	assert.False(t, hit)

	// new-key should still be there
	hit, err = cache.Restore(ctx, store, dest, "new-key", nil)
	require.NoError(t, err)
	assert.True(t, hit)
}
