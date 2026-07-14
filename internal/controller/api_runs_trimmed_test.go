package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// get performs an authenticated GET and returns (status, body).
func get(t *testing.T, s *Server, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestLogEndpoints_IdenticalAfterTrim is the feature's core guarantee: every
// windowed-viewer and CLI log endpoint returns byte-identical responses
// before and after the run's logs rows are trimmed from the DB.
func TestLogEndpoints_IdenticalAfterTrim(t *testing.T) {
	s, pg := newTestServer(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	s.SetObjectStore(obj)
	runID := seedParityRun(t, pg, obj) // from archived_logs_test.go
	ctx := context.Background()
	require.NoError(t, pg.CreateLogArchive(ctx, runID, runLogArchiveKey(runID), 1))
	require.NoError(t, pg.MarkRunFinished(ctx, runID, api.RunSucceeded))

	paths := []string{
		"/api/v1/runs/" + runID + "/logs?after=0",
		"/api/v1/runs/" + runID + "/logs/stats",
		"/api/v1/runs/" + runID + "/logs/stats?steps=0,2",
		"/api/v1/runs/" + runID + "/logs/range?offset=1&limit=3",
		"/api/v1/runs/" + runID + "/logs/range?steps=1,2&offset=0&limit=10",
		"/api/v1/runs/" + runID + "/logs/search?q=alpha",
		"/api/v1/runs/" + runID + "/logs/search?q=100%25&steps=0",
	}
	before := map[string]string{}
	for _, p := range paths {
		code, body := get(t, s, p)
		require.Equal(t, http.StatusOK, code, p)
		before[p] = body
	}

	n, err := pg.TrimRunLogs(ctx, runID)
	require.NoError(t, err)
	require.Positive(t, n)

	for _, p := range paths {
		code, body := get(t, s, p)
		require.Equal(t, http.StatusOK, code, p)
		assert.Equal(t, before[p], body, "response changed after trim: %s", p)
	}
}

// Timestamps: the JSON encoding of a time scanned from Postgres (microsecond
// precision) and one decoded from the archive ndjson written by the SAME
// archiver flow are identical, because seedParityRun builds the archive from
// TailLogs — the same source the pre-trim responses used. If this test flakes
// on timestamp formatting, compare decoded JSON instead of raw strings.

func TestLogEndpoints_TrimmedButObjectMissing_Returns503(t *testing.T) {
	s, pg := newTestServer(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	s.SetObjectStore(obj)
	runID := seedParityRun(t, pg, obj)
	ctx := context.Background()
	require.NoError(t, pg.CreateLogArchive(ctx, runID, runLogArchiveKey(runID), 1))
	require.NoError(t, pg.MarkRunFinished(ctx, runID, api.RunSucceeded))
	_, err := pg.TrimRunLogs(ctx, runID)
	require.NoError(t, err)
	require.NoError(t, obj.Delete(ctx, runLogArchiveKey(runID)))

	code, body := get(t, s, "/api/v1/runs/"+runID+"/logs/stats")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body, "log archive unavailable")
}

// TestTrimThenRetention_DeletesArchiveObject: run retention on a TRIMMED run
// must still remove the archive object and the run row (spec: combined
// trim -> retention case).
func TestTrimThenRetention_DeletesArchiveObject(t *testing.T) {
	s, pg := newTestServer(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	s.SetObjectStore(obj)
	runID := seedParityRun(t, pg, obj)
	ctx := context.Background()
	require.NoError(t, pg.CreateLogArchive(ctx, runID, runLogArchiveKey(runID), 1))
	require.NoError(t, pg.MarkRunFinished(ctx, runID, api.RunSucceeded))
	_, err := pg.TrimRunLogs(ctx, runID)
	require.NoError(t, err)

	require.NoError(t, deleteRunEverywhere(ctx, pg, obj, runID))

	_, err = obj.Get(ctx, runLogArchiveKey(runID))
	assert.ErrorIs(t, err, objectstore.ErrNotFound, "archive object must be gone")
	arch, err := pg.GetLogArchive(ctx, runID)
	require.NoError(t, err)
	assert.Nil(t, arch, "archive record must cascade away with the run")
}

func TestLogEndpoints_UntrimmedRunUnaffected(t *testing.T) {
	// An archive record WITHOUT trimmed_at must keep serving from the DB
	// even if the object store is empty.
	s, pg := newTestServer(t)
	s.SetObjectStore(objectstore.NewLocalObjectStore(t.TempDir()))
	runID := seedParityRun(t, pg, objectstore.NewLocalObjectStore(t.TempDir()))
	require.NoError(t, pg.CreateLogArchive(context.Background(), runID, runLogArchiveKey(runID), 1))

	code, _ := get(t, s, "/api/v1/runs/"+runID+"/logs/stats")
	assert.Equal(t, http.StatusOK, code)
}
