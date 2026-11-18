package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const agentVolumeYAML = `apiVersion: unified-cd/v1
kind: AgentVolume
metadata:
  name: unity-library
spec:
  mode: exclusive
  size: 200Gi
  storageClassName: fast
  agentSelector: [role:unity]
`

func applyAgentVolumeReq(t *testing.T, s *Server, y string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(api.ApplyAgentVolumeRequest{YAML: y})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-volumes", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func listAgentVolumesReq(t *testing.T, s *Server) []api.AgentVolumeMeta {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent-volumes", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list []api.AgentVolumeMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	return list
}

func TestAgentVolumeApplyListDelete(t *testing.T) {
	s, _ := newTestServer(t)

	rec := applyAgentVolumeReq(t, s, agentVolumeYAML)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var meta api.AgentVolumeMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &meta))
	assert.Equal(t, "unity-library", meta.Name)

	list := listAgentVolumesReq(t, s)
	require.Len(t, list, 1)
	assert.Equal(t, "unity-library", list[0].Name)
	assert.Equal(t, "exclusive", list[0].Spec.Mode)
	assert.Equal(t, "200Gi", list[0].Spec.Size)
	assert.Equal(t, "fast", list[0].Spec.StorageClassName)
	assert.Equal(t, []string{"role:unity"}, list[0].Spec.AgentSelector)
	assert.False(t, list[0].UpdatedAt.IsZero())

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agent-volumes/unity-library", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Empty(t, listAgentVolumesReq(t, s))
}

func TestAgentVolumeApplyInvalid(t *testing.T) {
	s, _ := newTestServer(t)
	rec := applyAgentVolumeReq(t, s, `apiVersion: unified-cd/v1
kind: AgentVolume
metadata:
  name: v
spec:
  mode: sideways
  size: 1Gi
`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "spec.mode")
	assert.Empty(t, listAgentVolumesReq(t, s))
}

func TestAgentVolumeManagedGuard(t *testing.T) {
	s, pg := newTestServer(t)
	manageResource(t, pg, "AgentVolume", "unity-library")

	rec := applyAgentVolumeReq(t, s, agentVolumeYAML)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `managed by AppSource "owner"`)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agent-volumes/unity-library", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}
