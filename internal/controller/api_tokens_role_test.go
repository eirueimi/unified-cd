package controller

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A role above the creator's global role is refused, never clamped.
func TestCreateToken_OverHighRoleIs400(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{RoleMap: map[string]string{"g-v": "viewer"}}, `
global:
  groups:
    "g-v": [token.create]
`)
	body, _ := json.Marshal(api.CreatePATRequest{Name: "ci", Role: "admin"})
	rec := doAs(t, s, http.MethodPost, "/api/v1/tokens", bearer("v@example.com", "g-v"), body)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "cannot grant role \"admin\": it exceeds your global role \"viewer\"; use --permission to grant folder permissions you hold\n", rec.Body.String())
	pats, err := pg.ListPATs(t.Context())
	require.NoError(t, err)
	assert.Empty(t, pats)
}

func TestCreateToken_UnknownRoleAndPermission(t *testing.T) {
	s, _ := newAuthzTestServer(t, authz.Legacy{RoleMap: map[string]string{"g-dev": "developer"}}, "")
	tok := bearer("d@example.com", "g-dev")
	body, _ := json.Marshal(api.CreatePATRequest{Name: "ci", Role: "root"})
	rec := doAs(t, s, http.MethodPost, "/api/v1/tokens", tok, body)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "unknown role \"root\"\n", rec.Body.String())

	body, _ = json.Marshal(api.CreatePATRequest{Name: "ci", Permissions: []string{"jobs.read"}})
	rec = doAs(t, s, http.MethodPost, "/api/v1/tokens", tok, body)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "unknown permission \"jobs.read\"\n", rec.Body.String())

	body, _ = json.Marshal(api.CreatePATRequest{Name: "ci", Role: "viewer", Permissions: []string{"job.read"}})
	rec = doAs(t, s, http.MethodPost, "/api/v1/tokens", tok, body)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "role and permissions are mutually exclusive\n", rec.Body.String())

	rec = doAs(t, s, http.MethodPost, "/api/v1/tokens", tok, []byte(`{"name":"ci","permissions":[]}`))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "permissions must not be empty\n", rec.Body.String())
}

// A role-only PAT creating a PAT stays role-only and is bounded by its own role.
func TestCreateToken_RoleOnlyPATCreatorIsRoleOnly(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, "")
	_, err := pg.CreatePAT(t.Context(), "dev", HashToken("exc_dev"), "developer", nil)
	require.NoError(t, err)

	body, _ := json.Marshal(api.CreatePATRequest{Name: "ci", Role: "admin"})
	rec := doAs(t, s, http.MethodPost, "/api/v1/tokens", "exc_dev", body)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `it exceeds your global role "developer"`)

	body, _ = json.Marshal(api.CreatePATRequest{Name: "ci", Role: "viewer"})
	rec = doAs(t, s, http.MethodPost, "/api/v1/tokens", "exc_dev", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp api.CreatePATResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	pat, err := pg.GetPATByHash(t.Context(), HashToken(resp.Token))
	require.NoError(t, err)
	assert.Equal(t, "viewer", pat.Role)
	assert.Nil(t, pat.Owner)
}
