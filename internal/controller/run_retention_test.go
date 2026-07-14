package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRetentionStore is a minimal store.Store stand-in implementing only the
// methods run retention uses (same pattern as fakeAuditRetentionStore).
type fakeRetentionStore struct {
	store.Store

	lockAcquired bool
	archives     map[string]*store.LogArchive // runID -> archive record (nil map = none)
	expired      [][]string                   // successive ListExpiredRuns results
	listCalls    int
	deleted      []string
}

func (f *fakeRetentionStore) AcquireAdvisoryLock(ctx context.Context, key int64) (func(), error) {
	if !f.lockAcquired {
		return nil, nil
	}
	return func() {}, nil
}

func (f *fakeRetentionStore) ListExpiredRuns(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	if f.listCalls >= len(f.expired) {
		return nil, nil
	}
	ids := f.expired[f.listCalls]
	f.listCalls++
	return ids, nil
}

func (f *fakeRetentionStore) GetLogArchive(ctx context.Context, runID string) (*store.LogArchive, error) {
	return f.archives[runID], nil
}

func (f *fakeRetentionStore) DeleteRun(ctx context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// failingObjStore wraps a real object store but fails every Delete.
type failingObjStore struct {
	objectstore.ObjectStore
}

func (f *failingObjStore) Delete(ctx context.Context, key string) error {
	return errors.New("object store down")
}

func TestDeleteRunEverywhere_DeletesObjectsThenRow(t *testing.T) {
	ctx := context.Background()
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	require.NoError(t, obj.Put(ctx, "runs/r1/logs.ndjson", strings.NewReader("{}"), 2))
	require.NoError(t, obj.Put(ctx, "artifacts/r1/out.tar.gz", strings.NewReader("x"), 1))
	require.NoError(t, obj.Put(ctx, "artifacts/r2/keep.tar.gz", strings.NewReader("x"), 1))
	st := &fakeRetentionStore{
		archives: map[string]*store.LogArchive{
			"r1": {RunID: "r1", ObjectKey: "runs/r1/logs.ndjson"},
		},
	}

	require.NoError(t, deleteRunEverywhere(ctx, st, obj, "r1"))

	assert.Equal(t, []string{"r1"}, st.deleted)
	_, err := obj.Get(ctx, "runs/r1/logs.ndjson")
	assert.ErrorIs(t, err, objectstore.ErrNotFound, "log archive object gone")
	keys, err := obj.List(ctx, "artifacts/r1/")
	require.NoError(t, err)
	assert.Empty(t, keys, "r1 artifacts gone")
	keys, err = obj.List(ctx, "artifacts/r2/")
	require.NoError(t, err)
	assert.Len(t, keys, 1, "other runs' artifacts untouched")
}

func TestDeleteRunEverywhere_ObjectFailureKeepsDBRow(t *testing.T) {
	ctx := context.Background()
	inner := objectstore.NewLocalObjectStore(t.TempDir())
	require.NoError(t, inner.Put(ctx, "runs/r1/logs.ndjson", strings.NewReader("{}"), 2))
	st := &fakeRetentionStore{
		archives: map[string]*store.LogArchive{
			"r1": {RunID: "r1", ObjectKey: "runs/r1/logs.ndjson"},
		},
	}

	err := deleteRunEverywhere(ctx, st, &failingObjStore{ObjectStore: inner}, "r1")

	assert.Error(t, err)
	assert.Empty(t, st.deleted, "DB row must survive an object-delete failure")
}

func TestDeleteRunEverywhere_NoRecordsIsIdempotent(t *testing.T) {
	// No archive record, no artifact objects: a retry after a partial
	// earlier attempt must still delete the DB row without error.
	ctx := context.Background()
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	st := &fakeRetentionStore{}

	require.NoError(t, deleteRunEverywhere(ctx, st, obj, "r1"))
	assert.Equal(t, []string{"r1"}, st.deleted)
}

func TestDeleteRunEverywhere_NilObjectStoreDeletesRow(t *testing.T) {
	ctx := context.Background()
	st := &fakeRetentionStore{}

	require.NoError(t, deleteRunEverywhere(ctx, st, nil, "r1"))
	assert.Equal(t, []string{"r1"}, st.deleted)
}
