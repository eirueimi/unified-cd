package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// registerVolumeAgent registers agentID through the HTTP handler, advertising
// volumes, and returns its bearer token.
func registerVolumeAgent(t *testing.T, s *Server, token, agentID string, volumes []string) {
	t.Helper()
	body, err := json.Marshal(api.AgentRegisterRequest{
		AgentID: agentID, Hostname: "h", OS: "linux",
		Capabilities: []string{dsl.CapNative}, Volumes: volumes,
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func claimAs(t *testing.T, s *Server, token, agentID string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/claim?timeout=300ms", rd)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

const volumeJobSpec = `{"native":true,"volumes":[{"name":["{{ .Params.LIB }}"],"as":"lib","mountPath":"."}],` +
	`"steps":[{"name":"s","run":"echo x"}]}`

func TestAgentAPI_Register_PersistsVolumes(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "va", nil, nil)
	registerVolumeAgent(t, s, token, "va", []string{"unity-main", "go-cache"})

	got, err := pg.GetAgent(t.Context(), "va")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.ElementsMatch(t, []string{"unity-main", "go-cache"}, got.Volumes)

	// Re-registering with none replaces the set: a volume dropped from the
	// agent's config stops being routable.
	registerVolumeAgent(t, s, token, "va", nil)
	got, err = pg.GetAgent(t.Context(), "va")
	require.NoError(t, err)
	assert.Empty(t, got.Volumes)
}

func TestAgentAPI_Claim_HonoursBusyVolumesFromTheBody(t *testing.T) {
	s, pg := newTestServer(t)
	_, err := pg.UpsertJob(t.Context(), "vj", "unified-cd/v1", []byte(volumeJobSpec))
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "vj", map[string]string{"LIB": "unity"}, []byte(volumeJobSpec), nil, nil, "", "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(t.Context(), 10)
	require.NoError(t, err)

	token := issueAgentAccessForTest(t, pg, "va", nil, nil)
	registerVolumeAgent(t, s, token, "va", []string{"unity"})

	// While the agent reports the volume busy, the run is not handed to it.
	busy, _ := json.Marshal(api.ClaimRequest{BusyVolumes: []string{"unity"}})
	rec := claimAs(t, s, token, "va", busy)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got api.ClaimResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Empty(t, got.RunID, "a run needing a busy volume must stay queued")

	// An absolute path is the agent's own bookkeeping and matches no volume
	// name, so reporting one does not hold a run back.
	paths, _ := json.Marshal(api.ClaimRequest{BusyVolumes: []string{"/go/cache"}})
	rec = claimAs(t, s, token, "va", paths)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, run.ID, got.RunID)

	// The response names the concrete volume the template expanded to, and
	// keeps the alias and mount path, so the agent can mount it.
	require.Len(t, got.Volumes, 1)
	assert.Equal(t, dsl.StringOrSlice{"unity"}, got.Volumes[0].Name)
	assert.Equal(t, "lib", got.Volumes[0].As)
	assert.Equal(t, ".", got.Volumes[0].MountPath)
}

func TestAgentAPI_Claim_AcceptsAnEmptyJSONBody(t *testing.T) {
	s, pg := newTestServer(t)
	_, err := pg.UpsertJob(t.Context(), "plain", "unified-cd/v1", []byte(`{"native":true,"steps":[{"name":"s","run":"echo x"}]}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "plain", nil, []byte(`{"native":true,"steps":[{"name":"s","run":"echo x"}]}`), nil, nil, "", "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(t.Context(), 10)
	require.NoError(t, err)
	token := issueAgentAccessForTest(t, pg, "va", nil, nil)

	rec := claimAs(t, s, token, "va", []byte(`{}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got api.ClaimResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, run.ID, got.RunID)
	assert.Empty(t, got.Volumes)
}

func TestAgentAPI_Claim_RejectsAMalformedBody(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "va", nil, nil)
	rec := claimAs(t, s, token, "va", []byte(`{not json`))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
