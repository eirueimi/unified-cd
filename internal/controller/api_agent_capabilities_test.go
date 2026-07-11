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
// validates every entry of Capabilities against dsl.ValidCapability and
// rejects the whole registration (400) on the first unrecognized one.
func TestRegister_RejectsUnknownCapability(t *testing.T) {
	s, _ := newTestServer(t)
	body, _ := json.Marshal(api.AgentRegisterRequest{
		AgentID: "a1", Hostname: "host1", OS: "linux",
		Capabilities: []string{"native", "gpu"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "unknown capability: gpu")
}

// TestRegister_PersistsCapabilities verifies that a registration with a valid
// capability set succeeds and the capabilities are visible via GetAgent.
func TestRegister_PersistsCapabilities(t *testing.T) {
	s, pg := newTestServer(t)
	body, _ := json.Marshal(api.AgentRegisterRequest{
		AgentID: "a1", Hostname: "host1", OS: "linux",
		Capabilities: []string{"native", "container"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	got, err := pg.GetAgent(context.Background(), "a1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.ElementsMatch(t, []string{"native", "container"}, got.Capabilities)
}

// TestRegister_EmptyCapabilitiesIsNotLegacy verifies the semantic distinction
// called out in the design: an agent registering with an explicit empty
// Capabilities slice ([]string{}) is NOT the same as a legacy agent (nil/NULL
// capabilities, which skips the capability check entirely on claim). An
// explicit empty set means "this agent can claim nothing capability-gated",
// and req.Capabilities must be threaded through as-is (nil stays nil, [] stays
// []) rather than coerced.
func TestRegister_EmptyCapabilitiesIsNotLegacy(t *testing.T) {
	s, pg := newTestServer(t)
	// Built as a raw JSON literal (not json.Marshal of the Go struct):
	// api.AgentRegisterRequest.Capabilities carries `json:"capabilities,omitempty"`,
	// so marshaling a Go []string{} would omit the field entirely and this test
	// would not actually exercise the empty-vs-nil distinction it's named for.
	body := []byte(`{"agentId":"a1","hostname":"host1","os":"linux","capabilities":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	got, err := pg.GetAgent(context.Background(), "a1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.NotNil(t, got.Capabilities, "an explicit empty Capabilities must persist as [] (non-legacy), not NULL")
	assert.Empty(t, got.Capabilities)
}
