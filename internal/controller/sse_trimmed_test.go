package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSE_TrimmedRunBackfillsFromArchive verifies a trimmed terminal run's
// SSE stream still replays its log lines (from the archive) followed by the
// terminal status event. The handler returns for terminal runs, so the
// request completes without needing to cancel the stream.
func TestSSE_TrimmedRunBackfillsFromArchive(t *testing.T) {
	s, pg := newTestServer(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	s.SetObjectStore(obj)
	runID := seedParityRun(t, pg, obj) // 6 lines, from archived_logs_test.go
	ctx := context.Background()
	lc, ms := archiveCoverage(t, pg, runID)
	require.NoError(t, pg.CreateLogArchive(ctx, runID, runLogArchiveKey(runID), 1, lc, ms))
	require.NoError(t, pg.MarkRunFinished(ctx, runID, api.RunSucceeded))
	_, err := pg.TrimRunLogs(ctx, runID)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID+"/events", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.Equal(t, 6, strings.Count(body, `"type":"log"`), "all archived lines replayed")
	assert.Contains(t, body, `"type":"status"`)
	assert.Contains(t, body, `"Succeeded"`)
	assert.Contains(t, body, "Building target ALPHA")
}
