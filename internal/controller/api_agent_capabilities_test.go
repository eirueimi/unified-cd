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
// validates every entry of the agent's self-reported capabilities (req.Capabilities)
// against dsl.ValidCapability and rejects the whole registration (400) on the
// first unrecognized one. The credential's authorized capabilities are left
// empty here since register no longer consults them.
func TestRegister_RejectsUnknownCapability(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)
	body, _ := json.Marshal(api.AgentRegisterRequest{AgentID: "a1", Hostname: "host1", OS: "linux", Capabilities: []string{"native", "gpu"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "unknown capability: gpu")
}

// TestRegister_PersistsReportedCapabilities verifies that handleAgentRegister
// persists the capabilities the agent itself reports in req.Capabilities (its
// own runtime auto-detection), not the credential's enrollment-time
// AuthorizedCapabilities. The enrolled identity here has an EMPTY authorized
// capability set, so a pre-change register (which seeded capabilities from
// principal.AuthorizedCapabilities) would persist no capabilities at all;
// this asserts the reported set lands instead.
func TestRegister_PersistsReportedCapabilities(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil) // empty authorized capabilities
	body, _ := json.Marshal(api.AgentRegisterRequest{AgentID: "a1", Hostname: "host1", OS: "linux", Capabilities: []string{"native", "container"}})
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

// TestRegister_ReportedCapabilities_OmitsUnreported is the second case from
// the task brief: an agent that reports only "native" (e.g. no container
// runtime detected) must NOT end up with "container" in its persisted
// capabilities, even though a differently-configured agent might report both.
func TestRegister_ReportedCapabilities_OmitsUnreported(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "a2", nil, nil) // empty authorized capabilities
	body, _ := json.Marshal(api.AgentRegisterRequest{AgentID: "a2", Hostname: "host2", OS: "linux", Capabilities: []string{"native"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	got, err := pg.GetAgent(context.Background(), "a2")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.ElementsMatch(t, []string{"native"}, got.Capabilities)
	assert.NotContains(t, got.Capabilities, "container")
}
