package controller

import (
	"context"
	"errors"
	"fmt"
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

func TestRunRetention_FollowerDoesNothing(t *testing.T) {
	st := &fakeRetentionStore{lockAcquired: false, expired: [][]string{{"r1"}}}
	runRunRetentionOnce(context.Background(), st, nil, 30)
	assert.Zero(t, st.listCalls)
	assert.Empty(t, st.deleted)
}

func TestRunRetention_ZeroDaysMeansKeepForever(t *testing.T) {
	// RunRunRetention (the looping entrypoint) must return immediately
	// without touching the store when retentionDays <= 0.
	st := &fakeRetentionStore{lockAcquired: true, expired: [][]string{{"r1"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	RunRunRetention(ctx, st, nil, 10*time.Millisecond, 0)
	assert.Zero(t, st.listCalls)
}

func TestRunRetention_DeletesExpiredAcrossBatches(t *testing.T) {
	// A full first batch (batch size 100) triggers an immediate second fetch
	// within the same tick; a short second batch ends the sweep.
	full := make([]string, runRetentionBatchSize)
	for i := range full {
		full[i] = fmt.Sprintf("run-%03d", i)
	}
	st := &fakeRetentionStore{
		lockAcquired: true,
		expired:      [][]string{full, {"run-last"}},
	}
	runRunRetentionOnce(context.Background(), st, nil, 30)
	assert.Equal(t, 2, st.listCalls)
	assert.Len(t, st.deleted, runRetentionBatchSize+1)
}

func TestRunRetention_ZeroProgressBatchStopsTick(t *testing.T) {
	// Failed runs stay in the oldest-first result set, so a full batch where
	// every delete fails must stop the tick instead of refetching the same
	// IDs forever. Deletes fail via an object store whose Delete errors.
	full := make([]string, runRetentionBatchSize)
	archives := make(map[string]*store.LogArchive, runRetentionBatchSize)
	for i := range full {
		id := fmt.Sprintf("run-%03d", i)
		full[i] = id
		archives[id] = &store.LogArchive{RunID: id, ObjectKey: "runs/" + id + "/logs.ndjson"}
	}
	inner := objectstore.NewLocalObjectStore(t.TempDir())
	st := &fakeRetentionStore{
		lockAcquired: true,
		archives:     archives,
		// The same full batch would be returned forever; the sweep must
		// stop after the first zero-progress batch.
		expired: [][]string{full, full, full},
	}
	runRunRetentionOnce(context.Background(), st, &failingObjStore{ObjectStore: inner}, 30)
	assert.Equal(t, 1, st.listCalls, "must not refetch after a zero-progress batch")
	assert.Empty(t, st.deleted)
}
