package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
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
	lc, ms := archiveCoverage(t, pg, runID)
	require.NoError(t, pg.CreateLogArchive(ctx, runID, runLogArchiveKey(runID), 1, lc, ms))
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
	lc, ms := archiveCoverage(t, pg, runID)
	require.NoError(t, pg.CreateLogArchive(ctx, runID, runLogArchiveKey(runID), 1, lc, ms))
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
	lc, ms := archiveCoverage(t, pg, runID)
	require.NoError(t, pg.CreateLogArchive(ctx, runID, runLogArchiveKey(runID), 1, lc, ms))
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

// TestAPI_GetRunSteps_ArtifactEntrySurvivesTrim covers Finding 2:
// handleGetRunSteps synthesizes the artifact-sidecar pseudo-step by checking
// s.store.CountLogs(ctx, id, []int{dsl.ArtifactLogIndex}) > 0, which becomes
// 0 once the run's logs rows are trimmed — the pseudo-step must not
// disappear from the steps panel just because the run got archived.
func TestAPI_GetRunSteps_ArtifactEntrySurvivesTrim(t *testing.T) {
	s, pg := newTestServer(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	s.SetObjectStore(obj)
	ctx := context.Background()

	specJSON := []byte(`{"steps":[{"name":"a","run":"echo a"}]}`)
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", specJSON)
	run, err := pg.CreateRun(ctx, "j", nil, specJSON, nil, nil, "api")
	require.NoError(t, err)
	_, err = pg.AppendLog(ctx, run.ID, dsl.ArtifactLogIndex, "stderr", time.Now(), "pushing artifact foo")
	require.NoError(t, err)

	getArtifactStep := func() *api.StepReport {
		t.Helper()
		code, body := get(t, s, "/api/v1/runs/"+run.ID+"/steps")
		require.Equal(t, http.StatusOK, code, body)
		var got []api.StepReport
		require.NoError(t, json.Unmarshal([]byte(body), &got))
		for i := range got {
			if got[i].Kind == "sidecar" && got[i].Name == "artifact" {
				return &got[i]
			}
		}
		return nil
	}

	before := getArtifactStep()
	require.NotNil(t, before, "artifact pseudo-step must be present before trim")
	assert.Equal(t, dsl.ArtifactLogIndex, before.Index)

	// Archive + trim, mirroring seedParityRun's archive-build step.
	lines, err := pg.TailLogs(ctx, run.ID, 0, 1_000_000)
	require.NoError(t, err)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, l := range lines {
		require.NoError(t, enc.Encode(l))
	}
	require.NoError(t, obj.Put(ctx, runLogArchiveKey(run.ID), &buf, int64(buf.Len())))
	lc, ms := archiveCoverage(t, pg, run.ID)
	require.NoError(t, pg.CreateLogArchive(ctx, run.ID, runLogArchiveKey(run.ID), 1, lc, ms))
	require.NoError(t, pg.MarkRunFinished(ctx, run.ID, api.RunSucceeded))
	_, err = pg.TrimRunLogs(ctx, run.ID)
	require.NoError(t, err)

	after := getArtifactStep()
	require.NotNil(t, after, "artifact pseudo-step must survive trim (served from the archive)")
	assert.Equal(t, dsl.ArtifactLogIndex, after.Index)
}

func TestLogEndpoints_UntrimmedRunUnaffected(t *testing.T) {
	// An archive record WITHOUT trimmed_at must keep serving from the DB
	// even if the object store is empty.
	s, pg := newTestServer(t)
	s.SetObjectStore(objectstore.NewLocalObjectStore(t.TempDir()))
	runID := seedParityRun(t, pg, objectstore.NewLocalObjectStore(t.TempDir()))
	lc, ms := archiveCoverage(t, pg, runID)
	require.NoError(t, pg.CreateLogArchive(context.Background(), runID, runLogArchiveKey(runID), 1, lc, ms))

	code, _ := get(t, s, "/api/v1/runs/"+runID+"/logs/stats")
	assert.Equal(t, http.StatusOK, code)
}
