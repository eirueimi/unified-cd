package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentAPI_SidecarStatus mirrors TestAgentAPI_LogBulk's harness: POST a
// SidecarStatusRequest to the agent-facing endpoint and assert it lands in
// the store, retrievable via GetSidecarStatuses.
func TestAgentAPI_SidecarStatus(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a1", run.ID)

	req := api.SidecarStatusRequest{RunID: run.ID, Name: "mysql", Index: 100, Phase: "running"}
	body, _ := json.Marshal(req)
	httpReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/a1/runs/"+run.ID+"/sidecars",
		bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httpReq)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	statuses, err := pg.GetSidecarStatuses(context.Background(), run.ID)
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, "mysql", statuses[0].Name)
	assert.Equal(t, 100, statuses[0].Index)
	assert.Equal(t, "running", statuses[0].Phase)
	assert.Nil(t, statuses[0].ExitCode)
}

// TestAgentAPI_SidecarStatus_UpsertsOnExit verifies a second report for the
// same (run, index) — the exited transition — updates the row in place
// rather than creating a duplicate.
func TestAgentAPI_SidecarStatus_UpsertsOnExit(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a1", run.ID)

	post := func(req api.SidecarStatusRequest) {
		body, _ := json.Marshal(req)
		httpReq := httptest.NewRequest(http.MethodPost,
			"/api/v1/agents/a1/runs/"+run.ID+"/sidecars",
			bytes.NewReader(body))
		httpReq.Header.Set("Authorization", "Bearer agent-secret")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, httpReq)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	}

	post(api.SidecarStatusRequest{RunID: run.ID, Name: "mysql", Index: 100, Phase: "running"})
	exitCode := 0
	post(api.SidecarStatusRequest{RunID: run.ID, Name: "mysql", Index: 100, Phase: "exited", ExitCode: &exitCode})

	statuses, err := pg.GetSidecarStatuses(context.Background(), run.ID)
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, "exited", statuses[0].Phase)
	require.NotNil(t, statuses[0].ExitCode)
	assert.Equal(t, 0, *statuses[0].ExitCode)
}
