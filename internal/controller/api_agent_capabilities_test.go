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

// TestRegister_RejectsUnknownCapability verifies that handleAgentRegister
// validates every entry of the principal's authorized capabilities against
// dsl.ValidCapability and rejects the whole registration (400) on the first
// unrecognized one. (The token is minted directly with an invalid authorized
// capability, bypassing the enrollment-policy validation, to exercise the
// register handler's own defense-in-depth check.)
func TestRegister_RejectsUnknownCapability(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "a1", nil, []string{"native", "gpu"})
	body, _ := json.Marshal(api.AgentRegisterRequest{AgentID: "a1", Hostname: "host1", OS: "linux"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "unknown capability: gpu")
}

// TestRegister_PersistsCapabilities verifies that a registration by an agent
// whose credential authorizes a valid capability set succeeds and those
// capabilities are visible via GetAgent.
func TestRegister_PersistsCapabilities(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "a1", nil, []string{"native", "container"})
	body, _ := json.Marshal(api.AgentRegisterRequest{AgentID: "a1", Hostname: "host1", OS: "linux"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	got, err := pg.GetAgent(context.Background(), "a1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.ElementsMatch(t, []string{"native", "container"}, got.Capabilities)
}
