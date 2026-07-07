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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAPI_StepLogs returns only the given step's lines (most recent `limit`,
// ascending seq) — the on-demand backfill for a step whose lines fell outside
// the SSE tail window on huge logs.
func TestAPI_StepLogs(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		_, err := pg.AppendLog(ctx, run.ID, 0, "stdout", now, fmt.Sprintf("zero-%d", i))
		require.NoError(t, err)
	}
	for i := 0; i < 5; i++ {
		_, err := pg.AppendLog(ctx, run.ID, 2, "stdout", now, fmt.Sprintf("two-%d", i))
		require.NoError(t, err)
	}

	get := func(path string) ([]api.LogLine, int) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		var lines []api.LogLine
		_ = json.Unmarshal(rec.Body.Bytes(), &lines)
		return lines, rec.Code
	}

	lines, code := get("/api/v1/runs/" + run.ID + "/steps/0/logs")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, lines, 3)
	assert.Equal(t, "zero-0", lines[0].Line)
	assert.Equal(t, 0, lines[0].StepIndex)

	// limit keeps the most recent lines of the step.
	lines, code = get("/api/v1/runs/" + run.ID + "/steps/2/logs?limit=2")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, lines, 2)
	assert.Equal(t, "two-3", lines[0].Line)

	// A step with no lines: empty JSON array, not null / not an error.
	lines, code = get("/api/v1/runs/" + run.ID + "/steps/7/logs")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, lines)
	assert.Empty(t, lines)

	// Non-numeric step index is a client error.
	_, code = get("/api/v1/runs/" + run.ID + "/steps/abc/logs")
	assert.Equal(t, http.StatusBadRequest, code)
}
