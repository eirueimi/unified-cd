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

func TestAPI_CreateToken(t *testing.T) {
	s, _ := newTestServer(t)
	body, _ := json.Marshal(api.CreatePATRequest{Name: "my-token"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp api.CreatePATResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.Token)
	assert.True(t, len(resp.Token) > 10)
	assert.Equal(t, "my-token", resp.Name)
}

func TestAPI_ListTokens(t *testing.T) {
	s, _ := newTestServer(t)
	body, _ := json.Marshal(api.CreatePATRequest{Name: "tok1"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(httptest.NewRecorder(), req)

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/tokens", nil)
	req2.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req2)
	require.Equal(t, http.StatusOK, rec.Code)
	var list []api.PATMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	// newTestServer also syncs a bootstrap PAT equivalent to UNIFIED_TOKEN, so the count is +1 over what was created.
	require.Len(t, list, 2)
	names := []string{list[0].Name, list[1].Name}
	assert.Contains(t, names, "tok1")
	assert.Contains(t, names, "test-bootstrap")
}

func TestAPI_PAT_CanAuthenticateAPI(t *testing.T) {
	s, _ := newTestServer(t)
	body, _ := json.Marshal(api.CreatePATRequest{Name: "ci-token"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	var resp api.CreatePATResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Verify that the issued PAT can be used to call the API.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req2.Header.Set("Authorization", "Bearer "+resp.Token)
	rec2 := httptest.NewRecorder()
	s.Router().ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusOK, rec2.Code, "should be able to call the API with the PAT")
}
