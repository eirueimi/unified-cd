package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
)

// fakeTrimStore is a minimal store.Store stand-in for the log-trim sweeper.
type fakeTrimStore struct {
	store.Store

	lockAcquired   bool
	candidates     [][]string // successive ListTrimCandidates results
	listCalls      int
	trimmed        []string
	deletedRecords []string
	trimErr        map[string]error
}

func (f *fakeTrimStore) AcquireAdvisoryLock(ctx context.Context, key int64) (func(), error) {
	if !f.lockAcquired {
		return nil, nil
	}
	return func() {}, nil
}

func (f *fakeTrimStore) ListTrimCandidates(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	if f.listCalls >= len(f.candidates) {
		return nil, nil
	}
	ids := f.candidates[f.listCalls]
	f.listCalls++
	return ids, nil
}

func (f *fakeTrimStore) TrimRunLogs(ctx context.Context, runID string) (int64, error) {
	if err := f.trimErr[runID]; err != nil {
		return 0, err
	}
	f.trimmed = append(f.trimmed, runID)
	return 1, nil
}

func (f *fakeTrimStore) DeleteLogArchive(ctx context.Context, runID string) error {
	f.deletedRecords = append(f.deletedRecords, runID)
	return nil
}

// seedArchiveObject writes a placeholder archive object for runID so the
// sweeper's existence check passes.
func seedArchiveObject(t *testing.T, obj objectstore.ObjectStore, runID string) {
	t.Helper()
	if err := obj.Put(context.Background(), runLogArchiveKey(runID),
		bytes.NewReader([]byte("{}")), 2); err != nil {
		t.Fatal(err)
	}
}

func TestLogTrim_FollowerDoesNothing(t *testing.T) {
	st := &fakeTrimStore{lockAcquired: false, candidates: [][]string{{"r1"}}}
	runLogTrimOnce(context.Background(), st, objectstore.NewLocalObjectStore(t.TempDir()), 7)
	assert.Zero(t, st.listCalls)
	assert.Empty(t, st.trimmed)
}

func TestLogTrim_DisabledOrNoObjectStore(t *testing.T) {
	st := &fakeTrimStore{lockAcquired: true, candidates: [][]string{{"r1"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	RunLogTrim(ctx, st, objectstore.NewLocalObjectStore(t.TempDir()), 10*time.Millisecond, 0)
	assert.Zero(t, st.listCalls, "trimDays<=0 must disable the loop")
	RunLogTrim(ctx, st, nil, 10*time.Millisecond, 7)
	assert.Zero(t, st.listCalls, "nil object store must disable the loop")
}

func TestLogTrim_TrimsCandidatesWithExistingObjects(t *testing.T) {
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	seedArchiveObject(t, obj, "r1")
	seedArchiveObject(t, obj, "r2")
	st := &fakeTrimStore{lockAcquired: true, candidates: [][]string{{"r1", "r2"}}}
	runLogTrimOnce(context.Background(), st, obj, 7)
	assert.Equal(t, []string{"r1", "r2"}, st.trimmed)
	assert.Empty(t, st.deletedRecords)
}

func TestLogTrim_MissingObjectDeletesStaleRecordAndSkipsTrim(t *testing.T) {
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	seedArchiveObject(t, obj, "r2")
	st := &fakeTrimStore{lockAcquired: true, candidates: [][]string{{"r1", "r2"}}}
	runLogTrimOnce(context.Background(), st, obj, 7)
	assert.Equal(t, []string{"r2"}, st.trimmed, "r1 must not be trimmed")
	assert.Equal(t, []string{"r1"}, st.deletedRecords, "r1's stale record must be deleted so the archiver re-archives")
}

func TestLogTrim_ZeroProgressBatchStopsTick(t *testing.T) {
	// Every candidate fails to trim and the same full batch would repeat
	// forever; the tick must stop after one zero-progress batch.
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	full := make([]string, logTrimBatchSize)
	trimErr := map[string]error{}
	for i := range full {
		id := fmt.Sprintf("run-%03d", i)
		full[i] = id
		seedArchiveObject(t, obj, id)
		trimErr[id] = errors.New("db down")
	}
	st := &fakeTrimStore{lockAcquired: true, candidates: [][]string{full, full}, trimErr: trimErr}
	runLogTrimOnce(context.Background(), st, obj, 7)
	assert.Equal(t, 1, st.listCalls)
	assert.Empty(t, st.trimmed)
}
