package cache_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// testObjectKey mirrors cache.objectKey (unexported), which this external
// test package cannot call directly. It must stay in lockstep with that
// function so tests can target the exact store key Restore will Get.
func testObjectKey(jobName, key string) string {
	j := sha256.Sum256([]byte(jobName))
	h := sha256.Sum256([]byte(key))
	return "caches/" + base64.RawURLEncoding.EncodeToString(j[:]) + "/" + base64.RawURLEncoding.EncodeToString(h[:])
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

	require.NoError(t, cache.Save(ctx, store, "test-job", src, "mykey", 7))

	hit, err := cache.Restore(ctx, store, "test-job", dest, "mykey", nil)
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
	require.NoError(t, cache.Save(ctx, store, "test-job", src, "npm-abc123", 7))

	// Restore with exact miss but prefix match
	dest := t.TempDir()
	hit, err := cache.Restore(ctx, store, "test-job", dest, "npm-xyz999", []string{"npm-"})
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

	require.NoError(t, cache.Save(ctx, base, "test-job", src, "npm-abc123", 7))

	exactKey := testObjectKey("test-job", "npm-xyz999") + ".tar.zst"
	store := &errKeyStore{ObjectStore: base, errKey: exactKey, err: fmt.Errorf("get object %q: %w", exactKey, objectstore.ErrNotFound)}

	dest := t.TempDir()
	hit, err := cache.Restore(ctx, store, "test-job", dest, "npm-xyz999", []string{"npm-"})
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
	require.NoError(t, cache.Save(ctx, base, "test-job", src, "npm-abc123", 7))

	exactKey := testObjectKey("test-job", "npm-xyz999") + ".tar.zst"
	transientErr := errors.New("transient network error")
	store := &errKeyStore{ObjectStore: base, errKey: exactKey, err: transientErr}

	dest := t.TempDir()
	// ...but the exact-key Get fails with a non-NotFound error, so the
	// fallback must never be attempted: Restore propagates the error.
	hit, err := cache.Restore(ctx, store, "test-job", dest, "npm-xyz999", []string{"npm-"})
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

	require.NoError(t, cache.Save(ctx, base, "test-job", src, "npm-abc123", 7))

	fallbackKey := testObjectKey("test-job", "npm-abc123") + ".tar.zst"
	transientErr := errors.New("transient network error")
	store := &errKeyStore{ObjectStore: base, errKey: fallbackKey, err: transientErr}

	dest := t.TempDir()
	hit, err := cache.Restore(ctx, store, "test-job", dest, "npm-xyz999", []string{"npm-"})
	assert.False(t, hit)
	require.Error(t, err)
	assert.ErrorIs(t, err, transientErr)
	assert.NotErrorIs(t, err, cache.ErrCacheMiss)
}

func TestCache_Restore_MissNoFallback(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	dest := t.TempDir()

	hit, err := cache.Restore(ctx, store, "test-job", dest, "missing-key", nil)
	assert.False(t, hit)
	assert.ErrorIs(t, err, cache.ErrCacheMiss)
}

// putFailingStore fails the Nth Put (1-based) and delegates everything else.
type putFailingStore struct {
	objectstore.ObjectStore
	puts    int
	failPut int
}

func (f *putFailingStore) Put(ctx context.Context, key string, content io.Reader, size int64) error {
	f.puts++
	if f.puts == f.failPut {
		return errors.New("meta put failed")
	}
	return f.ObjectStore.Put(ctx, key, content, size)
}

// TestSave_MetaFailureDeletesArchive: a failed .meta Put must not leave an
// orphaned .tar.zst (GC and lookup only ever iterate .meta objects).
func TestSave_MetaFailureDeletesArchive(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644))
	inner := objectstore.NewLocalObjectStore(t.TempDir())
	st := &putFailingStore{ObjectStore: inner, failPut: 2} // archive Put ok, meta Put fails

	err := cache.Save(context.Background(), st, "test-job", dir, "key1", 7)
	require.Error(t, err)

	keys, err := inner.List(context.Background(), "")
	require.NoError(t, err)
	assert.Empty(t, keys, "no orphaned object may survive a failed save")
}

// archiveSizeSpyStore records the size passed to Put for the .tar.zst archive
// key (ignoring the small .meta Put) and delegates to an embedded real store.
type archiveSizeSpyStore struct {
	objectstore.ObjectStore
	archiveSize int64
}

func (s *archiveSizeSpyStore) Put(ctx context.Context, key string, content io.Reader, size int64) error {
	if strings.HasSuffix(key, ".tar.zst") {
		s.archiveSize = size
	}
	return s.ObjectStore.Put(ctx, key, content, size)
}

func TestSave_StreamsArchiveAndCountsMetaSize(t *testing.T) {
	ctx := context.Background()
	spy := &archiveSizeSpyStore{ObjectStore: objectstore.NewLocalObjectStore(t.TempDir())}
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "f.txt"), bytes.Repeat([]byte("z"), 4096), 0o600))

	require.NoError(t, cache.Save(ctx, spy, "test-job", src, "mykey", 7))

	// The archive Put must be streamed (unknown size), NOT the buffered length.
	assert.Equal(t, int64(-1), spy.archiveSize, "archive must be streamed with size -1")

	// Independently compute the archive length; Meta.Size must equal it.
	var buf bytes.Buffer
	require.NoError(t, artifact.WriteTarZstd(&buf, src))
	wantSize := int64(buf.Len())

	oKey := testObjectKey("test-job", "mykey")
	rc, err := spy.Get(ctx, oKey+".meta")
	require.NoError(t, err)
	defer rc.Close()
	var m cache.Meta
	require.NoError(t, json.NewDecoder(rc).Decode(&m))
	assert.Equal(t, wantSize, m.Size)
	assert.Equal(t, "mykey", m.OriginalKey)
}

func TestCache_DeleteExpired_RemovesOldKeepsNew(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	src := makeDir(t, map[string]string{"f.txt": "data"})

	require.NoError(t, cache.Save(ctx, store, "test-job", src, "old-key", 1))
	require.NoError(t, cache.Save(ctx, store, "test-job", src, "new-key", 30))

	// Delete entries expiring before "now + 2 days" (removes old-key, keeps new-key)
	n, err := cache.DeleteExpired(ctx, store, time.Now().Add(2*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// old-key should be gone
	dest := t.TempDir()
	hit, _ := cache.Restore(ctx, store, "test-job", dest, "old-key", nil)
	assert.False(t, hit)

	// new-key should still be there
	hit, err = cache.Restore(ctx, store, "test-job", dest, "new-key", nil)
	require.NoError(t, err)
	assert.True(t, hit)
}

// TestRestore_CannotHijackAnotherJobsEntry is the regression test for the
// cross-job cache-poisoning defect (C-1): job A saves an entry under a key
// crafted to match job B's restoreKeys prefix, with a long TTL (both
// attacker-controlled). Before namespacing, findBestMatch scanned every job's
// .meta objects and job B's Restore would happily extract job A's archive
// into its own workspace. Now job B's List is scoped to its own jobPrefix, so
// job A's entry is never even enumerated.
func TestRestore_CannotHijackAnotherJobsEntry(t *testing.T) {
	ctx := context.Background()
	store := objectstore.NewLocalObjectStore(t.TempDir())

	// Job A plants an entry with a long TTL under a key job B's restoreKeys prefix-match.
	srcA := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcA, "pwned.txt"), []byte("evil"), 0o600))
	require.NoError(t, cache.Save(ctx, store, "attacker/job", srcA, "deps-pwned", 3650))

	// Job B restores with a prefix that would have matched job A's key. With
	// no entries of its own, this is a genuine miss: Restore reports it via
	// the same ErrCacheMiss sentinel every other true-miss path in this
	// package uses (see TestCache_Restore_MissNoFallback) — the point of this
	// test is that job A's entry is never surfaced as a hit, not that the
	// error value differs from an ordinary miss.
	dest := t.TempDir()
	hit, err := cache.Restore(ctx, store, "victim/job", dest, "deps-victim", []string{"deps-"})
	assert.ErrorIs(t, err, cache.ErrCacheMiss)
	assert.False(t, hit, "a job must never restore another job's cache entry")
	_, statErr := os.Stat(filepath.Join(dest, "pwned.txt"))
	assert.Error(t, statErr, "attacker payload must not land in the victim workspace")
}

// TestSaveRestore_SameJobRoundTrips proves the namespacing change did not
// break the ordinary same-job round trip: Save then exact-key Restore, both
// under the same qualified job name.
func TestSaveRestore_SameJobRoundTrips(t *testing.T) {
	ctx := context.Background()
	store := objectstore.NewLocalObjectStore(t.TempDir())
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("data"), 0o600))
	require.NoError(t, cache.Save(ctx, store, "team-a/build", src, "deps-v1", 7))

	dest := t.TempDir()
	hit, err := cache.Restore(ctx, store, "team-a/build", dest, "deps-v1", nil)
	require.NoError(t, err)
	assert.True(t, hit)
	got, err := os.ReadFile(filepath.Join(dest, "f.txt"))
	require.NoError(t, err)
	assert.Equal(t, "data", string(got))
}

// TestRestoreKeys_FallbackWorksWithinSameJob proves the restoreKeys prefix
// fallback still works when the candidate and the restorer are the same job
// — namespacing must not collapse the fallback into an always-miss.
func TestRestoreKeys_FallbackWorksWithinSameJob(t *testing.T) {
	ctx := context.Background()
	store := objectstore.NewLocalObjectStore(t.TempDir())
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("data"), 0o600))
	require.NoError(t, cache.Save(ctx, store, "team-a/build", src, "deps-abc123", 7))

	dest := t.TempDir()
	hit, err := cache.Restore(ctx, store, "team-a/build", dest, "deps-nomatch", []string{"deps-"})
	require.NoError(t, err)
	assert.True(t, hit, "prefix fallback must still work within the same job")
}
