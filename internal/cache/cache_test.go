package cache_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// testObjectKey mirrors cache.objectKey (unexported), which this external
// test package cannot call directly. It must stay in lockstep with that
// function so tests can target the exact store key Restore will Get.
func testObjectKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return "caches/" + base64.RawURLEncoding.EncodeToString(h[:])
}

func newStore(t *testing.T) objectstore.ObjectStore {
	t.Helper()
	return objectstore.NewLocalObjectStore(t.TempDir())
}

// errKeyStore wraps an ObjectStore and forces Get(errKey) to fail with a
// caller-supplied, non-ErrNotFound error, regardless of whether that key
// actually exists in the wrapped store. It exists to simulate a transient
// backend failure (e.g. a network error) distinct from a genuine miss, since
// LocalObjectStore only ever produces ErrNotFound or nil.
type errKeyStore struct {
	objectstore.ObjectStore
	errKey string
	err    error
}

func (s *errKeyStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == s.errKey {
		return nil, s.err
	}
	return s.ObjectStore.Get(ctx, key)
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

// TestCache_Restore_ExactKeyErrNotFound_FallsBackToRestoreKeys is the
// regression test for the live bug: against a store whose exact-key Get
// returns ErrNotFound (mirroring real S3/minio-go's NoSuchKey response once
// eagerly detected — the fake wraps ErrNotFound explicitly here so the test
// doesn't rely on LocalObjectStore's own detection), but a restoreKeys
// prefix matches a saved entry, Restore must fall back to it and hit.
func TestCache_Restore_ExactKeyErrNotFound_FallsBackToRestoreKeys(t *testing.T) {
	ctx := context.Background()
	base := newStore(t)
	src := makeDir(t, map[string]string{"pkg.json": "v1"})

	require.NoError(t, cache.Save(ctx, base, src, "npm-abc123", 7))

	exactKey := testObjectKey("npm-xyz999") + ".tar.zst"
	store := &errKeyStore{ObjectStore: base, errKey: exactKey, err: fmt.Errorf("get object %q: %w", exactKey, objectstore.ErrNotFound)}

	dest := t.TempDir()
	hit, err := cache.Restore(ctx, store, dest, "npm-xyz999", []string{"npm-"})
	require.NoError(t, err)
	assert.True(t, hit)

	got, err := os.ReadFile(filepath.Join(dest, "pkg.json"))
	require.NoError(t, err)
	assert.Equal(t, "v1", string(got))
}

// TestCache_Restore_ExactKeyNonNotFoundError_Propagates ensures a transient
// (non-ErrNotFound) failure on the exact-key Get is NOT masked by the
// restoreKeys fallback, even when a valid fallback candidate exists — a
// network error must surface as an error, not silently look like a miss.
func TestCache_Restore_ExactKeyNonNotFoundError_Propagates(t *testing.T) {
	ctx := context.Background()
	base := newStore(t)
	src := makeDir(t, map[string]string{"pkg.json": "v1"})

	// A valid fallback candidate exists...
	require.NoError(t, cache.Save(ctx, base, src, "npm-abc123", 7))

	exactKey := testObjectKey("npm-xyz999") + ".tar.zst"
	transientErr := errors.New("transient network error")
	store := &errKeyStore{ObjectStore: base, errKey: exactKey, err: transientErr}

	dest := t.TempDir()
	// ...but the exact-key Get fails with a non-NotFound error, so the
	// fallback must never be attempted: Restore propagates the error.
	hit, err := cache.Restore(ctx, store, dest, "npm-xyz999", []string{"npm-"})
	assert.False(t, hit)
	require.Error(t, err)
	assert.ErrorIs(t, err, transientErr)
	assert.NotErrorIs(t, err, cache.ErrCacheMiss)
}

// TestCache_Restore_FallbackKeyNonNotFoundError_Propagates mirrors the above
// for the fallback fetch itself: once findBestMatch identifies a candidate,
// a non-NotFound error fetching it must propagate rather than collapse into
// ErrCacheMiss.
func TestCache_Restore_FallbackKeyNonNotFoundError_Propagates(t *testing.T) {
	ctx := context.Background()
	base := newStore(t)
	src := makeDir(t, map[string]string{"pkg.json": "v1"})

	require.NoError(t, cache.Save(ctx, base, src, "npm-abc123", 7))

	fallbackKey := testObjectKey("npm-abc123") + ".tar.zst"
	transientErr := errors.New("transient network error")
	store := &errKeyStore{ObjectStore: base, errKey: fallbackKey, err: transientErr}

	dest := t.TempDir()
	hit, err := cache.Restore(ctx, store, dest, "npm-xyz999", []string{"npm-"})
	assert.False(t, hit)
	require.Error(t, err)
	assert.ErrorIs(t, err, transientErr)
	assert.NotErrorIs(t, err, cache.ErrCacheMiss)
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
