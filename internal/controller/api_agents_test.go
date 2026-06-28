package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPI_ListAgents(t *testing.T) {
	s, pg := newTestServer(t)
	require.NoError(t, pg.UpsertAgent(t.Context(), "ag1", "host1", "linux", "dev", []string{"kind:linux"}, nil))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var agents []api.AgentInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &agents))
	require.Len(t, agents, 1)
	assert.Equal(t, "ag1", agents[0].ID)
}
