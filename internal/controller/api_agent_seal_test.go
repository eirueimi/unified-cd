package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sealRun creates a finished run with one stored log line and an archive
// record, i.e. a run whose logs are sealed.
func sealRun(t *testing.T, st store.Store) string {
	t.Helper()
	ctx := context.Background()
	_, err := st.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := st.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	seq, err := st.AppendLog(ctx, run.ID, 0, "stdout", time.Now(), "real line")
	require.NoError(t, err)
	require.NoError(t, st.MarkRunFinished(ctx, run.ID, api.RunSucceeded))
	require.NoError(t, st.CreateLogArchive(ctx, run.ID, "runs/"+run.ID+"/logs.ndjson", 1, 1, seq))
	return run.ID
}

func TestAgentLogAppend_SealedRunDropsLine(t *testing.T) {
	s, st := newTestServer(t)
	runID := sealRun(t, st)

	body, _ := json.Marshal(api.LogAppendRequest{RunID: runID, StepIndex: 0, Stream: "stdout", Line: "late line"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/logs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	count, _, _, err := st.CountLogs(context.Background(), runID, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "late line must not be stored")
}

func TestAgentLogBulk_SealedRunDropsLines(t *testing.T) {
	s, st := newTestServer(t)
	runID := sealRun(t, st)

	lines := []api.LogAppendRequest{
		{RunID: runID, StepIndex: 1, Stream: "stdout", Line: "late 1"},
		{RunID: runID, StepIndex: 1, Stream: "stderr", Line: "late 2"},
	}
	body, _ := json.Marshal(lines)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/runs/"+runID+"/steps/1/logs/bulk", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	count, _, _, err := st.CountLogs(context.Background(), runID, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}

func TestArtifactUpload_NonexistentRun404(t *testing.T) {
	s, _ := newTestServer(t)
	s.SetObjectStore(objectstore.NewLocalObjectStore(t.TempDir()))

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/runs/00000000-0000-0000-0000-000000000000/artifacts/out",
		strings.NewReader("data"))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}
