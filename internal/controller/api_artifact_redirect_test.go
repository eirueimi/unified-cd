package controller

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func redirectTestServer(t *testing.T, mode string) (s *Server, obj *fakePresigner, path string, payload []byte, agentToken string) {
	t.Helper()
	s, st := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, artifactDownloadPolicy)
	s.cfg.ArtifactDownload = mode
	obj = newFakePresigner()
	s.SetObjectStore(obj)
	payload = []byte{0x00, 0x1f, 0x8b, 0x08, 0xff, 'a'}
	runID := seedClaimedArtifactRun(t, st, obj, "team-a/build", "artifact-owner", "a", payload)
	agentToken = issueAgentAccessForTest(t, st, "artifact-other-agent", nil, nil)
	return s, obj, "/api/v1/runs/" + runID + "/artifacts/a", payload, agentToken
}

func auditCount(t *testing.T, s *Server) int {
	t.Helper()
	logs, err := s.store.ListAuditLogs(t.Context(), 100, 0)
	require.NoError(t, err)
	return len(logs)
}

func TestArtifactDownload_RedirectForHumans(t *testing.T) {
	s, obj, path, _, _ := redirectTestServer(t, "redirect")
	before := auditCount(t, s)

	rec := doAs(t, s, http.MethodGet, path, bearer("viewer@example.com", "g-a"), nil)
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Header().Get("Location"), "https://store/artifacts/")
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
	require.Len(t, obj.calls, 1)
	assert.Equal(t, "get", obj.calls[0].Op)
	assert.Equal(t, "a.tar.gz", obj.calls[0].Filename)
	// Same audit footprint as proxy mode (successful reads add no row).
	assert.Equal(t, before, auditCount(t, s))

	t.Run("missing object is 404", func(t *testing.T) {
		rec := doAs(t, s, http.MethodGet, path[:len(path)-1]+"zzz", bearer("viewer@example.com", "g-a"), nil)
		require.Equal(t, http.StatusNotFound, rec.Code)
		assert.Empty(t, rec.Header().Get("Location"))
	})

	t.Run("no permission is still hidden", func(t *testing.T) {
		rec := doAs(t, s, http.MethodGet, path, bearer("viewer@example.com", "g-b"), nil)
		require.Equal(t, http.StatusNotFound, rec.Code)
		assert.Empty(t, rec.Header().Get("Location"))
	})
}

func TestArtifactDownload_RedirectModeStillProxiesAgents(t *testing.T) {
	s, obj, path, payload, agentToken := redirectTestServer(t, "redirect")
	rec := doAs(t, s, http.MethodGet, path, agentToken, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, payload, rec.Body.Bytes())
	assert.Empty(t, obj.calls, "agents must not get presigned URLs here")

	rec = doAs(t, s, http.MethodGet, path+"?as=url", agentToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, payload, rec.Body.Bytes())
}

func TestArtifactDownload_AsURLJSON(t *testing.T) {
	s, _, path, _, _ := redirectTestServer(t, "redirect")
	rec := doAs(t, s, http.MethodGet, path+"?as=url", bearer("viewer@example.com", "g-a"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
	var got struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Contains(t, got.URL, "https://store/artifacts/")
}

func TestArtifactDownload_ProxyDefault(t *testing.T) {
	for _, mode := range []string{"", "proxy"} {
		s, obj, path, payload, _ := redirectTestServer(t, mode)
		rec := doAs(t, s, http.MethodGet, path+"?as=url", bearer("viewer@example.com", "g-a"), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, payload, rec.Body.Bytes(), "mode %q ignores ?as=url and streams", mode)
		assert.Empty(t, rec.Header().Get("Location"))
		assert.Empty(t, obj.calls)
	}
}
