package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeArchiverStore is a minimal store.Store stand-in implementing only the
// methods archiveRunLogs uses (same pattern as fakeRetentionStore in
// run_retention_test.go), so CreateLogArchive's failure path can be forced
// without needing a real FK violation against Postgres.
type fakeArchiverStore struct {
	store.Store

	lines             []api.LogLine
	createArchiveErr  error
	createArchiveCall struct {
		runID, key           string
		size, lineCount, seq int64
	}

	// runs is returned by every ListRunsNeedingArchival call (excluded is
	// recorded but not applied — the filtering itself is covered by the
	// Postgres-backed TestExcludedParam test).
	runs          []api.Run
	excludedCalls [][]string

	// tailErrIDs, if set, makes TailLogs fail for the given run ids,
	// simulating archiveRunLogs failing for a poison candidate.
	tailErrIDs map[string]bool
}

func (f *fakeArchiverStore) ListRunsNeedingArchival(ctx context.Context, limit int, excluded []string) ([]api.Run, error) {
	f.excludedCalls = append(f.excludedCalls, excluded)
	return f.runs, nil
}

func (f *fakeArchiverStore) TailLogs(ctx context.Context, runID string, afterSeq int64, limit int) ([]api.LogLine, error) {
	if f.tailErrIDs[runID] {
		return nil, errors.New("tail logs failed")
	}
	return f.lines, nil
}

func (f *fakeArchiverStore) CreateLogArchive(ctx context.Context, runID, objectKey string, sizeBytes, lineCount, maxSeq int64) error {
	f.createArchiveCall.runID = runID
	f.createArchiveCall.key = objectKey
	f.createArchiveCall.size = sizeBytes
	f.createArchiveCall.lineCount = lineCount
	f.createArchiveCall.seq = maxSeq
	return f.createArchiveErr
}

func TestRunLogArchiver_OnlyOneLeaderArchives(t *testing.T) {
	pg := store.NewTestPostgres(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())

	// Create a completed Run.
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	_ = pg.MarkRunRunning(t.Context(), run.ID)
	_ = pg.MarkRunFinished(t.Context(), run.ID, api.RunSucceeded)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start two RunLogArchiver instances concurrently.
	go RunLogArchiver(ctx, pg, obj, 50*time.Millisecond)
	go RunLogArchiver(ctx, pg, obj, 50*time.Millisecond)

	// Wait until archival completes.
	require.Eventually(t, func() bool {
		archive, _ := pg.GetLogArchive(t.Context(), run.ID)
		return archive != nil
	}, 3*time.Second, 100*time.Millisecond)

	// There should be exactly one archive record (no duplicates).
	archive, err := pg.GetLogArchive(t.Context(), run.ID)
	require.NoError(t, err)
	require.NotNil(t, archive)
}

// TestArchiveRunLogs_CreateRecordFailureDeletesUploadedObject covers the
// archiver side of the archiver/sweeper race (see deleteRunEverywhere's doc
// comment in run_retention.go): if CreateLogArchive fails after the object
// was already Put (e.g. the run was deleted concurrently and the FK on
// run_id rejects the insert), the just-uploaded object must not be left
// behind as an orphan.
func TestArchiveRunLogs_CreateRecordFailureDeletesUploadedObject(t *testing.T) {
	ctx := context.Background()
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	st := &fakeArchiverStore{
		lines:            []api.LogLine{{Seq: 1, Line: "hello"}},
		createArchiveErr: errors.New("insert or update on table \"run_log_archives\" violates foreign key constraint"),
	}

	err := archiveRunLogs(ctx, st, obj, "r1")

	require.Error(t, err)
	assert.Equal(t, "r1", st.createArchiveCall.runID)
	_, getErr := obj.Get(ctx, "runs/r1/logs.ndjson")
	assert.ErrorIs(t, getErr, objectstore.ErrNotFound, "orphaned object cleaned up after CreateLogArchive failure")
}

// TestArchiveRunLogs_CreateRecordFailureCleanupAlsoFails exercises the
// warn-log-only branch: even if the compensating Delete itself fails,
// archiveRunLogs still returns the original CreateLogArchive error rather
// than panicking or masking it.
func TestArchiveRunLogs_CreateRecordFailureCleanupAlsoFails(t *testing.T) {
	ctx := context.Background()
	inner := objectstore.NewLocalObjectStore(t.TempDir())
	st := &fakeArchiverStore{
		lines:            []api.LogLine{{Seq: 1, Line: "hello"}},
		createArchiveErr: errors.New("fk violation"),
	}

	err := archiveRunLogs(ctx, st, &failingObjStore{ObjectStore: inner}, "r1")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "fk violation")
}

// TestArchivePendingLogs_PoisonCandidateExcludedNextTick: a run whose
// archiveRunLogs call fails must be excluded from the next tick's
// ListRunsNeedingArchival call, while a succeeding run is not (Success
// forgets it immediately).
func TestArchivePendingLogs_PoisonCandidateExcludedNextTick(t *testing.T) {
	ctx := context.Background()
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	st := &fakeArchiverStore{
		lines:      []api.LogLine{{Seq: 1, Line: "hello"}},
		runs:       []api.Run{{ID: "poison"}, {ID: "ok"}},
		tailErrIDs: map[string]bool{"poison": true},
	}
	bo := newFailureBackoff(time.Minute, time.Hour, 10_000)

	require.NoError(t, archivePendingLogs(ctx, st, obj, bo))
	require.Len(t, st.excludedCalls, 1)
	assert.Empty(t, st.excludedCalls[0], "first tick excludes nothing")

	require.NoError(t, archivePendingLogs(ctx, st, obj, bo))
	require.Len(t, st.excludedCalls, 2)
	assert.Contains(t, st.excludedCalls[1], "poison", "second tick excludes the poison candidate")
	assert.NotContains(t, st.excludedCalls[1], "ok", "a succeeding run is not excluded")
}
