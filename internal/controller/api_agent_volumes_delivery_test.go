package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
)

func putAgentVolume(t *testing.T, pg store.Store, name, spec string) {
	t.Helper()
	require.NoError(t, pg.UpsertAgentVolume(t.Context(), name, []byte(spec)))
}

func decodeHeartbeat(t *testing.T, rr *httptest.ResponseRecorder) api.HeartbeatResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var out api.HeartbeatResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	return out
}

func TestHeartbeatReturnsDesiredVolumes(t *testing.T) {
	s, pg := newTestServer(t)
	putAgentVolume(t, pg, "a", `{"mode":"exclusive","size":"1Gi","agentSelector":["role:unity"],"reclaimPolicy":"Delete"}`)
	putAgentVolume(t, pg, "b", `{"mode":"shared","size":"2Gi","storageClassName":"fast"}`)
	putAgentVolume(t, pg, "c", `{"mode":"shared","size":"3Gi","agentSelector":["role:ios"]}`)
	token := issueAgentAccessForTest(t, pg, "hb1", []string{"role:unity"}, nil)

	first := decodeHeartbeat(t, postHeartbeatForTest(t, s, "hb1", token, &api.HeartbeatRequest{ActiveRunIDs: []string{}}))
	assert.Equal(t, []api.DesiredVolume{
		{Name: "a", Mode: "exclusive", Size: "1Gi", ReclaimPolicy: "Delete"},
		{Name: "b", Mode: "shared", Size: "2Gi", StorageClassName: "fast", ReclaimPolicy: "Retain"},
	}, first.Volumes)
	assert.Len(t, first.Revision, 64)

	second := decodeHeartbeat(t, postHeartbeatForTest(t, s, "hb1", token, &api.HeartbeatRequest{ActiveRunIDs: []string{}}))
	assert.Equal(t, first.Revision, second.Revision)

	putAgentVolume(t, pg, "a", `{"mode":"exclusive","size":"5Gi","agentSelector":["role:unity"]}`)
	third := decodeHeartbeat(t, postHeartbeatForTest(t, s, "hb1", token, &api.HeartbeatRequest{ActiveRunIDs: []string{}}))
	assert.NotEqual(t, first.Revision, third.Revision)
	assert.Equal(t, "5Gi", third.Volumes[0].Size)
}

func TestHeartbeatEmptySetIsNonNilArray(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "hb-empty", nil, nil)
	rr := postHeartbeatForTest(t, s, "hb-empty", token, &api.HeartbeatRequest{ActiveRunIDs: []string{}})
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"volumes":[]`)
}

func TestHeartbeatBodylessStillReturnsSet(t *testing.T) {
	s, pg := newTestServer(t)
	putAgentVolume(t, pg, "b", `{"mode":"shared","size":"2Gi"}`)
	token := issueAgentAccessForTest(t, pg, "hb2", nil, nil)

	out := decodeHeartbeat(t, postHeartbeatForTest(t, s, "hb2", token, nil))
	require.Len(t, out.Volumes, 1)
	assert.Equal(t, "b", out.Volumes[0].Name)
}

type volumeListFailingStore struct{ store.Store }

func (volumeListFailingStore) ListAgentVolumes(context.Context) ([]store.AgentVolumeRecord, error) {
	return nil, errors.New("list failed")
}

func TestHeartbeatStoreErrorReturns503(t *testing.T) {
	_, pg := newTestServer(t)
	s := NewServer(Config{}, volumeListFailingStore{Store: pg})
	t.Cleanup(s.Close)
	token := issueAgentAccessForTest(t, pg, "hb3", nil, nil)

	rr := postHeartbeatForTest(t, s, "hb3", token, &api.HeartbeatRequest{ActiveRunIDs: []string{}})
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Empty(t, rr.Body.String())
}

func agentVolumeStatusOf(t *testing.T, s *Server, id string) []api.AgentVolumeStatus {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var agents []api.AgentInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &agents))
	for _, a := range agents {
		if a.ID == id {
			return a.VolumeStatus
		}
	}
	t.Fatalf("agent %s not listed", id)
	return nil
}

func TestHeartbeatPersistsVolumeStatus(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "hb4", nil, nil)
	want := []api.AgentVolumeStatus{{Name: "a", Ready: false, Reason: "claim name too long"}}

	rr := postHeartbeatForTest(t, s, "hb4", token, &api.HeartbeatRequest{ActiveRunIDs: []string{}, VolumeStatus: want})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, err := pg.GetAgent(t.Context(), "hb4")
	require.NoError(t, err)
	assert.Equal(t, want, got.VolumeStatus)

	rr = postHeartbeatForTest(t, s, "hb4", token, &api.HeartbeatRequest{ActiveRunIDs: []string{}})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, err = pg.GetAgent(t.Context(), "hb4")
	require.NoError(t, err)
	assert.Empty(t, got.VolumeStatus)
}

func TestAgentsListShowsVolumeStatus(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "hb5", nil, nil)
	want := []api.AgentVolumeStatus{{Name: "a", Ready: false, Reason: "claim name too long"}}

	rr := postHeartbeatForTest(t, s, "hb5", token, &api.HeartbeatRequest{ActiveRunIDs: []string{}, VolumeStatus: want})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, want, agentVolumeStatusOf(t, s, "hb5"))

	rr = postHeartbeatForTest(t, s, "hb5", token, &api.HeartbeatRequest{ActiveRunIDs: []string{}})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Empty(t, agentVolumeStatusOf(t, s, "hb5"))
}

func TestClaimWithoutVolumesKnownKeepsStoredVolumes(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "cl1", nil, nil)
	registerVolumeAgent(t, s, token, "cl1", []string{"a", "b"})

	body, _ := json.Marshal(api.ClaimRequest{})
	rec := claimAs(t, s, token, "cl1", body)
	require.Less(t, rec.Code, 300, rec.Body.String())

	got, err := pg.GetAgent(t.Context(), "cl1")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, got.Volumes)
}

func TestClaimWithVolumesKnownClearsVolumes(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "cl2", nil, nil)
	registerVolumeAgent(t, s, token, "cl2", []string{"a", "b"})

	body, _ := json.Marshal(api.ClaimRequest{VolumesKnown: true})
	rec := claimAs(t, s, token, "cl2", body)
	require.Less(t, rec.Code, 300, rec.Body.String())

	got, err := pg.GetAgent(t.Context(), "cl2")
	require.NoError(t, err)
	assert.Empty(t, got.Volumes)
}

func TestRegisterWithoutVolumesKnownKeepsStoredVolumes(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "rg1", nil, nil)
	registerVolumeAgent(t, s, token, "rg1", []string{"a", "b"})

	registerVolumeAgent(t, s, token, "rg1", nil)
	got, err := pg.GetAgent(t.Context(), "rg1")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, got.Volumes, "a register that has no volumes and is not authoritative keeps the stored list")
}

func TestRegisterWithVolumesKnownClearsVolumes(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "rg2", nil, nil)
	registerVolumeAgent(t, s, token, "rg2", []string{"a"})

	body, _ := json.Marshal(api.AgentRegisterRequest{AgentID: "rg2", Hostname: "h", OS: "linux", VolumesKnown: true})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	got, err := pg.GetAgent(t.Context(), "rg2")
	require.NoError(t, err)
	assert.Empty(t, got.Volumes)
}
