package controller

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTokenAs(t *testing.T, s *Server, tok string, req api.CreatePATRequest) api.CreatePATResponse {
	t.Helper()
	rec := doAs(t, s, http.MethodPost, "/api/v1/tokens", tok, createTokenBody(req))
	return decodeCreated(t, rec)
}

func patByID(t *testing.T, pg store.Store, id string) store.PAT {
	t.Helper()
	pats, err := pg.ListPATs(t.Context())
	require.NoError(t, err)
	for _, p := range pats {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("pat %s not found", id)
	return store.PAT{}
}

// The token.create row keeps the PAT name as its resource and carries the new
// PAT's id in details, so "pat:<id>" in later rows maps back to this creation.
// A service-account caller adds its account to the details.
func TestCreateToken_AuditDetailsCarryPATID(t *testing.T) {
	s, pg := newTestServer(t)

	resp := createTokenAs(t, s, "secret", api.CreatePATRequest{Name: "ci", Role: "viewer"})

	logs, err := pg.ListAuditLogs(t.Context(), 1, 0)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	row := logs[0]
	secretPAT, err := pg.GetPATByHash(t.Context(), HashToken("secret"))
	require.NoError(t, err)
	assert.Equal(t, "token.create", row.Action)
	assert.Equal(t, "ci", row.Resource)
	assert.Equal(t, "pat:"+secretPAT.ID, row.ActorID)
	var d map[string]any
	require.NoError(t, json.Unmarshal(row.Details, &d))
	assert.Equal(t, map[string]any{"patId": resp.ID, "name": "ci", "account": "sa:test-admin"}, d)
	assert.NotContains(t, string(row.Details), resp.Token)
}

// A refused or invalid create has no PAT, so it records no PAT details (only
// the service account).
func TestCreateToken_AuditNoDetailsOnFailure(t *testing.T) {
	s, pg := newTestServer(t)

	rec := doAs(t, s, http.MethodPost, "/api/v1/tokens", "secret", createTokenBody(api.CreatePATRequest{Name: "env:UNIFIED_TOKEN"}))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	logs, err := pg.ListAuditLogs(t.Context(), 1, 0)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	var d map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Details, &d))
	assert.Equal(t, map[string]any{"account": "sa:test-admin"}, d)
}

func TestCreateToken_RecordsCreatedBy(t *testing.T) {
	s, pg := newTestServer(t)
	secretPAT, err := pg.GetPATByHash(t.Context(), HashToken("secret"))
	require.NoError(t, err)

	// A service-account PAT creates a PAT: created by that PAT.
	a := createTokenAs(t, s, "secret", api.CreatePATRequest{Name: "a", Role: "admin"})
	assert.Equal(t, "pat:"+secretPAT.ID, patByID(t, pg, a.ID).CreatedBy)

	// A PAT creates a role PAT and an enrollment-only PAT.
	b := createTokenAs(t, s, a.Token, api.CreatePATRequest{Name: "b", Role: "developer"})
	assert.Equal(t, "pat:"+a.ID, patByID(t, pg, b.ID).CreatedBy)
	c := createTokenAs(t, s, a.Token, api.CreatePATRequest{Name: "c", Permissions: []string{"agent.enroll"}})
	assert.Equal(t, "pat:"+a.ID, patByID(t, pg, c.ID).CreatedBy)

	// Rows created directly stay NULL.
	assert.Empty(t, secretPAT.CreatedBy)

	// A token list exposes it.
	rec := doAs(t, s, http.MethodGet, "/api/v1/tokens", "secret", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list []api.PATMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	got := map[string]string{}
	for _, m := range list {
		got[m.Name] = m.CreatedBy
	}
	assert.Equal(t, "pat:"+secretPAT.ID, got["a"])
	assert.Equal(t, "pat:"+a.ID, got["b"])
	assert.Equal(t, "", got[testAdminAccount])
}

func TestCreateToken_RecordsCreatedByOIDCOwner(t *testing.T) {
	s, pg := newAuthzTestServer(t, tokenLegacy, "")
	addSession(t, pg, "sess", "sub-dev", "dev@example.com", "developer", []string{"g-dev"})
	rec := doWithCookie(t, s, http.MethodPost, "/api/v1/tokens", "sess", createTokenBody(api.CreatePATRequest{Name: "mine", Role: "developer"}))
	resp := decodeCreated(t, rec)
	assert.Equal(t, "oidc:sub-dev", patByID(t, pg, resp.ID).CreatedBy)

	// PAT-from-PAT: the owner PAT is the creator.
	child := createTokenAs(t, s, resp.Token, api.CreatePATRequest{Name: "child"})
	assert.Equal(t, "pat:"+resp.ID, patByID(t, pg, child.ID).CreatedBy)

	logs, err := pg.ListAuditLogs(t.Context(), 1, 0)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	var d map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Details, &d))
	assert.Equal(t, child.ID, d["patId"])
}
