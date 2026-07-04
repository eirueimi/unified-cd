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

// a developer creating an admin PAT gets clamped to developer.
func TestCreateToken_ClampsToCreatorRole(t *testing.T) {
	s, pg := newTestServer(t)
	_, err := pg.CreatePAT(t.Context(), "dev", HashToken("exc_dev"), "developer", nil)
	require.NoError(t, err)

	body, _ := json.Marshal(api.CreatePATRequest{Name: "ci", Role: "admin"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer exc_dev")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.CreatePATResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	pat, err := pg.GetPATByHash(t.Context(), HashToken(resp.Token))
	require.NoError(t, err)
	assert.Equal(t, "developer", pat.Role)
}
