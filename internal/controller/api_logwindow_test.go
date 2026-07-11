package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAPI_LogWindow covers the three windowed-viewer endpoints end to end
// (real PG + router): stats, ranged fetch by view row number, and server
// search with ILIKE-literal semantics.
func TestAPI_LogWindow(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		_, _ = pg.AppendLog(ctx, run.ID, 0, "stdout", now, fmt.Sprintf("zero-%d", i))
	}
	for i := 0; i < 5; i++ {
		_, _ = pg.AppendLog(ctx, run.ID, 2, "stdout", now, fmt.Sprintf("two-%d", i))
	}

	getJSON := func(path string, into any) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), into))
		}
		return rec.Code
	}

	var stats struct{ Count, MinSeq, MaxSeq int64 }
	require.Equal(t, http.StatusOK, getJSON("/api/v1/runs/"+run.ID+"/logs/stats", &stats))
	assert.EqualValues(t, 8, stats.Count)
	require.Equal(t, http.StatusOK, getJSON("/api/v1/runs/"+run.ID+"/logs/stats?steps=2", &stats))
	assert.EqualValues(t, 5, stats.Count)

	var lines []api.LogLine
	require.Equal(t, http.StatusOK, getJSON("/api/v1/runs/"+run.ID+"/logs/range?offset=2&limit=3", &lines))
	require.Len(t, lines, 3)
	assert.Equal(t, "zero-2", lines[0].Line)
	require.Equal(t, http.StatusOK, getJSON("/api/v1/runs/"+run.ID+"/logs/range?steps=2&limit=2", &lines))
	assert.Equal(t, "two-0", lines[0].Line)
	assert.Equal(t, http.StatusBadRequest, getJSON("/api/v1/runs/"+run.ID+"/logs/range?offset=-1", &lines))
	assert.Equal(t, http.StatusBadRequest, getJSON("/api/v1/runs/"+run.ID+"/logs/range?steps=abc", &lines))

	var sr struct {
		Total   int64                  `json:"total"`
		Matches []store.LogSearchMatch `json:"matches"`
	}
	require.Equal(t, http.StatusOK, getJSON("/api/v1/runs/"+run.ID+"/logs/search?q=two-", &sr))
	assert.EqualValues(t, 5, sr.Total)
	assert.EqualValues(t, 3, sr.Matches[0].Row)
	assert.Equal(t, http.StatusBadRequest, getJSON("/api/v1/runs/"+run.ID+"/logs/search", &sr))

	// 旧エンドポイントは消えている
	code := getJSON("/api/v1/runs/"+run.ID+"/steps/0/logs", &lines)
	assert.Equal(t, http.StatusNotFound, code)
}
