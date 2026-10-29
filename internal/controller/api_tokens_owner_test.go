package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tokenFolderPolicy = `
global:
  groups:
    "g-fo": [token.create]
    "g-tm": [token.manage]
folders:
  team-a:
    groups:
      "g-fo": [job.read]
`

var tokenLegacy = authz.Legacy{RoleMap: map[string]string{"g-dev": "developer", "g-admin": "admin"}}

func doWithCookie(t *testing.T, s *Server, method, path, session string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func createTokenBody(req api.CreatePATRequest) []byte {
	b, _ := json.Marshal(req)
	return b
}

func decodeCreated(t *testing.T, rec *httptest.ResponseRecorder) api.CreatePATResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp api.CreatePATResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func TestCreateToken_PreUpgradeSessionRefused(t *testing.T) {
	s, pg := newAuthzTestServer(t, tokenLegacy, "")
	addSession(t, pg, "old-session", "old@example.com", "old@example.com", "developer", nil)
	rec := doWithCookie(t, s, http.MethodPost, "/api/v1/tokens", "old-session", createTokenBody(api.CreatePATRequest{Name: "ci"}))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "cannot create a token: this login predates group-aware tokens; log out and log in again, then retry\n", rec.Body.String())
	pats, err := pg.ListPATs(t.Context())
	require.NoError(t, err)
	assert.Empty(t, pats)
}

func TestCreateToken_BearerWithoutGroupsRefusedWhenGroupKeyed(t *testing.T) {
	legacy := authz.Legacy{RoleMap: map[string]string{"g-dev": "developer"}, DefaultRole: "developer"}
	s, pg := newAuthzTestServer(t, legacy, "")
	rec := doAs(t, s, http.MethodPost, "/api/v1/tokens", bearer("nogroups@example.com"), createTokenBody(api.CreatePATRequest{Name: "ci"}))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "cannot create a token: your OIDC token carries no \"groups\" claim; log in again so the token includes it, then retry\n", rec.Body.String())
	pats, err := pg.ListPATs(t.Context())
	require.NoError(t, err)
	assert.Empty(t, pats)
}

func TestCreateToken_BearerWithoutGroupsUserMapOnly(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{UserMap: map[string]string{"u@example.com": "developer"}}, "")
	rec := doAs(t, s, http.MethodPost, "/api/v1/tokens", bearer("u@example.com"), createTokenBody(api.CreatePATRequest{Name: "ci"}))
	resp := decodeCreated(t, rec)
	pat, err := pg.GetPATByHash(t.Context(), HashToken(resp.Token))
	require.NoError(t, err)
	require.NotNil(t, pat.Owner)
	assert.Equal(t, "u@example.com", pat.Owner.Sub)
	assert.Equal(t, "u@example.com", pat.Owner.Email)
	assert.Equal(t, []string{}, pat.Owner.Claims)
}

func TestCreateToken_FolderOnlyUserRoleIs400None(t *testing.T) {
	s, _ := newAuthzTestServer(t, tokenLegacy, tokenFolderPolicy)
	rec := doAs(t, s, http.MethodPost, "/api/v1/tokens", bearer("fo@example.com", "g-fo"), createTokenBody(api.CreatePATRequest{Name: "ci", Role: "developer"}))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "cannot grant role \"developer\": it exceeds your global role (none); use --permission to grant folder permissions you hold\n", rec.Body.String())
}

func TestCreateToken_UnheldPermissionIs400(t *testing.T) {
	s, pg := newAuthzTestServer(t, tokenLegacy, "")
	rec := doAs(t, s, http.MethodPost, "/api/v1/tokens", bearer("d@example.com", "g-dev"), createTokenBody(api.CreatePATRequest{Name: "ci", Permissions: []string{"secret.write"}}))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "cannot grant permission \"secret.write\": you do not hold it\n", rec.Body.String())
	pats, err := pg.ListPATs(t.Context())
	require.NoError(t, err)
	assert.Empty(t, pats)
}

func TestCreateToken_FolderOnlyUserNoCeiling(t *testing.T) {
	s, pg := newAuthzTestServer(t, tokenLegacy, tokenFolderPolicy)
	rec := doAs(t, s, http.MethodPost, "/api/v1/tokens", bearer("fo@example.com", "g-fo"), createTokenBody(api.CreatePATRequest{Name: "ci"}))
	resp := decodeCreated(t, rec)
	assert.Equal(t, "", resp.Role)
	pat, err := pg.GetPATByHash(t.Context(), HashToken(resp.Token))
	require.NoError(t, err)
	require.NotNil(t, pat.Owner)
	assert.Equal(t, "fo@example.com", pat.Owner.Sub)
	assert.Equal(t, []string{"g-fo"}, pat.Owner.Claims)
	assert.Nil(t, pat.Permissions)

	bodySeedJob(t, pg, "team-a/x")
	bodySeedJob(t, pg, "team-b/x")
	assert.Equal(t, http.StatusOK, doAs(t, s, http.MethodGet, "/api/v1/jobs/team-a/x", resp.Token, nil).Code)
	// A read the token may not make is hidden as not found.
	assert.Equal(t, http.StatusNotFound, doAs(t, s, http.MethodGet, "/api/v1/jobs/team-b/x", resp.Token, nil).Code)
}

func TestCreateToken_PATFromPATBoundByParent(t *testing.T) {
	s, pg := newAuthzTestServer(t, tokenLegacy, "")
	mustOwnedPAT(t, pg, "parent", "", []string{"job.read", "token.create"}, []string{"g-dev"})

	// The parent does not hold run.trigger within its ceiling.
	rec := doAs(t, s, http.MethodPost, "/api/v1/tokens", "parent", createTokenBody(api.CreatePATRequest{Name: "c1", Permissions: []string{"run.trigger"}}))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "cannot grant permission \"run.trigger\": you do not hold it\n", rec.Body.String())

	// No requested ceiling: inherits the parent's.
	resp := decodeCreated(t, doAs(t, s, http.MethodPost, "/api/v1/tokens", "parent", createTokenBody(api.CreatePATRequest{Name: "c2"})))
	assert.Equal(t, []string{"job.read", "token.create"}, resp.Permissions)
	child, err := pg.GetPATByHash(t.Context(), HashToken(resp.Token))
	require.NoError(t, err)
	require.NotNil(t, child.Owner)
	assert.Equal(t, "sub-parent", child.Owner.Sub)
	assert.Equal(t, []string{"g-dev"}, child.Owner.Claims)

	// A narrower requested ceiling is stored as a permission list.
	resp = decodeCreated(t, doAs(t, s, http.MethodPost, "/api/v1/tokens", "parent", createTokenBody(api.CreatePATRequest{Name: "c3", Permissions: []string{"job.read"}})))
	assert.Equal(t, []string{"job.read"}, resp.Permissions)
	assert.Equal(t, "", resp.Role)
}

func TestCreateToken_PATFromRoleCeilingPAT(t *testing.T) {
	s, pg := newAuthzTestServer(t, tokenLegacy, "")
	mustOwnedPAT(t, pg, "dev-parent", "developer", nil, []string{"g-dev"})

	// Inherits the parent's role ceiling as-is.
	resp := decodeCreated(t, doAs(t, s, http.MethodPost, "/api/v1/tokens", "dev-parent", createTokenBody(api.CreatePATRequest{Name: "c1"})))
	assert.Equal(t, "developer", resp.Role)
	assert.Empty(t, resp.Permissions)

	// A requested role is stored as bundle intersected with the parent ceiling.
	resp = decodeCreated(t, doAs(t, s, http.MethodPost, "/api/v1/tokens", "dev-parent", createTokenBody(api.CreatePATRequest{Name: "c2", Role: "viewer"})))
	viewer, _ := authz.Bundle("viewer")
	assert.Equal(t, "", resp.Role)
	assert.Equal(t, viewer.Names(), resp.Permissions)

	rec := doAs(t, s, http.MethodPost, "/api/v1/tokens", "dev-parent", createTokenBody(api.CreatePATRequest{Name: "c3", Role: "admin"}))
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateToken_BreakGlassCreatesRoleOnly(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, "")
	s.cfg.Token = "bg-token"
	_, err := pg.UpsertBootstrapPAT(t.Context(), BootstrapPATName, HashToken("bg-token"))
	require.NoError(t, err)

	resp := decodeCreated(t, doAs(t, s, http.MethodPost, "/api/v1/tokens", "bg-token", createTokenBody(api.CreatePATRequest{Name: "ci"})))
	pat, err := pg.GetPATByHash(t.Context(), HashToken(resp.Token))
	require.NoError(t, err)
	assert.Nil(t, pat.Owner)
	assert.Equal(t, "admin", pat.Role)

	rec := doAs(t, s, http.MethodPost, "/api/v1/tokens", "bg-token", createTokenBody(api.CreatePATRequest{Name: "ci2", Permissions: []string{"job.read"}}))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "cannot grant permissions: this token has no owner; use --role\n", rec.Body.String())
}

func TestTokens_DeveloperListsAndDeletesOwnOnly(t *testing.T) {
	s, pg := newAuthzTestServer(t, tokenLegacy, "")
	ctx := t.Context()
	addSession(t, pg, "dev-session", "dev@example.com", "dev@example.com", "developer", []string{"g-dev"})
	own, err := pg.CreateOwnedPAT(ctx, store.NewOwnedPAT{Name: "own", TokenHash: "h-own", Role: "viewer",
		Owner: store.PATOwner{Sub: "dev@example.com", Email: "dev@example.com", Claims: []string{"g-dev"}}})
	require.NoError(t, err)
	adminPAT, err := pg.CreateOwnedPAT(ctx, store.NewOwnedPAT{Name: "admins", TokenHash: "h-adm",
		Owner: store.PATOwner{Sub: "adm@example.com", Claims: []string{"g-admin"}}})
	require.NoError(t, err)
	roleOnly, err := pg.CreatePAT(ctx, "legacy", "h-legacy", "admin", nil)
	require.NoError(t, err)

	rec := doWithCookie(t, s, http.MethodGet, "/api/v1/tokens", "dev-session", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list []api.PATMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 1)
	assert.Equal(t, own.ID, list[0].ID)
	assert.Equal(t, "dev@example.com", list[0].Owner)

	for _, id := range []string{adminPAT.ID, roleOnly.ID} {
		rec = doWithCookie(t, s, http.MethodDelete, "/api/v1/tokens/"+id, "dev-session", nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		assert.Equal(t, "token not found\n", rec.Body.String())
	}
	all, err := pg.ListPATs(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 3)

	rec = doWithCookie(t, s, http.MethodDelete, "/api/v1/tokens/"+own.ID, "dev-session", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	all, err = pg.ListPATs(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 2)
}

func TestTokens_ManageListsAndDeletesAll(t *testing.T) {
	s, pg := newAuthzTestServer(t, tokenLegacy, "")
	ctx := t.Context()
	_, err := pg.CreateOwnedPAT(ctx, store.NewOwnedPAT{Name: "dev-pat", TokenHash: "h1", Role: "viewer",
		Owner: store.PATOwner{Sub: "dev@example.com", Claims: []string{"g-dev"}}})
	require.NoError(t, err)
	roleOnly, err := pg.CreatePAT(ctx, "legacy", "h2", "developer", nil)
	require.NoError(t, err)

	tok := bearer("adm@example.com", "g-admin")
	rec := doAs(t, s, http.MethodGet, "/api/v1/tokens", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list []api.PATMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 2)

	rec = doAs(t, s, http.MethodDelete, "/api/v1/tokens/"+roleOnly.ID, tok, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	all, err := pg.ListPATs(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 1)
}

// A principal holding token.manage but not token.create must reach the handlers (AltPerm).
func TestTokens_ManageOnlyPrincipal(t *testing.T) {
	s, pg := newAuthzTestServer(t, tokenLegacy, tokenFolderPolicy)
	ctx := t.Context()
	victim, err := pg.CreatePAT(ctx, "legacy", "h2", "developer", nil)
	require.NoError(t, err)
	_, err = pg.CreateOwnedPAT(ctx, store.NewOwnedPAT{Name: "other", TokenHash: "h3", Role: "viewer",
		Owner: store.PATOwner{Sub: "x@example.com", Claims: []string{"g-dev"}}})
	require.NoError(t, err)

	tok := bearer("tm@example.com", "g-tm")
	rec := doAs(t, s, http.MethodGet, "/api/v1/tokens", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list []api.PATMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Len(t, list, 2)

	rec = doAs(t, s, http.MethodDelete, "/api/v1/tokens/"+victim.ID, tok, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// token.manage alone does not allow creating tokens.
	rec = doAs(t, s, http.MethodPost, "/api/v1/tokens", tok, createTokenBody(api.CreatePATRequest{Name: "ci"}))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// An owner identity with an empty sub can neither create, list nor delete owner PATs.
func TestTokens_EmptySubIsRefused(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, "")
	ctx := t.Context()
	other, err := pg.CreateOwnedPAT(ctx, store.NewOwnedPAT{Name: "other", TokenHash: "h1", Role: "viewer",
		Owner: store.PATOwner{Sub: "x@example.com"}})
	require.NoError(t, err)

	p := Principal{Name: "nosub", Kind: "oidc", Role: "developer", ClaimsKnown: true, Claims: []string{},
		Grants: authz.RoleGrants("developer", "test")}
	call := func(h http.HandlerFunc, method, path, id string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		rc := chi.NewRouteContext()
		rc.URLParams.Add("id", id)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rc))
		rec := httptest.NewRecorder()
		h(rec, withPrincipal(req, p))
		return rec
	}

	rec := call(s.handleCreateToken, http.MethodPost, "/api/v1/tokens", "", createTokenBody(api.CreatePATRequest{Name: "ci"}))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = call(s.handleListTokens, http.MethodGet, "/api/v1/tokens", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String())
	rec = call(s.handleDeleteToken, http.MethodDelete, "/api/v1/tokens/"+other.ID, other.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	all, err := pg.ListPATs(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, other.ID, all[0].ID)
	// The store never matches an empty owner either.
	deleted, err := pg.DeleteOwnedPAT(ctx, other.ID, "")
	require.NoError(t, err)
	assert.False(t, deleted)
}

func TestWarnRoleOnlyPATs(t *testing.T) {
	ctx := context.Background()
	newLog := func() (*slog.Logger, *bytes.Buffer) {
		var buf bytes.Buffer
		return slog.New(slog.NewTextHandler(&buf, nil)), &buf
	}
	s, pg := newAuthzTestServer(t, tokenLegacy, tokenFolderPolicy)
	for _, c := range []struct{ name, role string }{
		{"a", "developer"}, {"b", "admin"}, {"v", "viewer"}, {BootstrapPATName, "admin"},
	} {
		_, err := pg.CreatePAT(ctx, c.name, HashToken("h-"+c.name), c.role, nil)
		require.NoError(t, err)
	}
	_, err := pg.CreateOwnedPAT(ctx, store.NewOwnedPAT{Name: "owned", TokenHash: "ho", Role: "admin", Owner: store.PATOwner{Sub: "s"}})
	require.NoError(t, err)

	log, buf := newLog()
	require.NoError(t, WarnRoleOnlyPATs(ctx, pg, s.oidcCfg.Policy, log))
	assert.Contains(t, buf.String(), `msg="role-only personal access tokens with a role above viewer bypass folder grants until revoked"`)
	assert.Contains(t, buf.String(), "count=2")
	assert.Contains(t, buf.String(), "level=WARN")

	// Without folder grants nothing is logged.
	s2, pg2 := newAuthzTestServer(t, tokenLegacy, "")
	_, err = pg2.CreatePAT(ctx, "a", HashToken("h-a"), "admin", nil)
	require.NoError(t, err)
	log, buf = newLog()
	require.NoError(t, WarnRoleOnlyPATs(ctx, pg2, s2.oidcCfg.Policy, log))
	assert.Empty(t, buf.String())
	log, buf = newLog()
	require.NoError(t, WarnRoleOnlyPATs(ctx, pg2, nil, log))
	assert.Empty(t, buf.String())
}

// A token id that is not a UUID is a missing token, not a server error.
func TestTokens_MalformedIDIsNotFound(t *testing.T) {
	s, _ := newAuthzTestServer(t, tokenLegacy, tokenFolderPolicy)
	for _, tok := range []string{bearer("adm@example.com", "g-admin"), bearer("dev@example.com", "g-dev")} {
		rec := doAs(t, s, http.MethodDelete, "/api/v1/tokens/not-a-uuid", tok, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		assert.Equal(t, "token not found\n", rec.Body.String())
	}
}
