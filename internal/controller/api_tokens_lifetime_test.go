package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/eirueimi/unified-cd/internal/store/storetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// addPermServicePAT adds a service account holding the viewer bundle plus
// extra global permissions to the running server's config and stores a PAT
// with the given token for it.
func addPermServicePAT(t testing.TB, s *Server, st store.Store, account, token string, extra ...authz.Permission) *store.PAT {
	t.Helper()
	cfg := withTestAdmin(s.cfg)
	set, _ := authz.Bundle("viewer")
	for _, p := range extra {
		set = set.With(p)
	}
	var g authz.Grants
	g.Add("", set, "sa:"+account)
	cfg.ServiceAccountGrants[account] = g
	s.cfg = cfg
	syncTestServiceAccounts(t, st, cfg)
	return seedServicePAT(t, st, account, token)
}

func postTokenReq(t *testing.T, s *Server, token string, req api.CreatePATRequest) (int, string, api.CreatePATResponse) {
	t.Helper()
	rec := doAs(t, s, http.MethodPost, "/api/v1/tokens", token, createTokenBody(req))
	var resp api.CreatePATResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec.Code, rec.Body.String(), resp
}

func expiryOf(t *testing.T, st store.Store, token string) *time.Time {
	t.Helper()
	pat, err := st.GetPATByHash(context.Background(), HashToken(token))
	require.NoError(t, err)
	require.NotNil(t, pat)
	return pat.ExpiresAt
}

func newLifetimeServer(t *testing.T, def, max time.Duration) (*Server, store.Store) {
	t.Helper()
	pg := storetest.NewTestPostgres(t)
	s := newServerWithAdminPAT(t, pg, Config{TokenDefaultLifetime: def, TokenMaxLifetime: max})
	t.Cleanup(s.Close)
	return s, pg
}

func TestTokenDefaultAndMaxLifetime(t *testing.T) {
	t.Run("default applies", func(t *testing.T) {
		s, pg := newLifetimeServer(t, 2160*time.Hour, 8760*time.Hour)
		code, body, resp := postTokenReq(t, s, "secret", api.CreatePATRequest{Name: "a"})
		require.Equal(t, http.StatusOK, code, body)
		exp := expiryOf(t, pg, resp.Token)
		require.NotNil(t, exp)
		assert.WithinDuration(t, time.Now().Add(2160*time.Hour), *exp, time.Minute)
	})
	t.Run("over max is refused", func(t *testing.T) {
		s, _ := newLifetimeServer(t, 2160*time.Hour, 8760*time.Hour)
		code, body, _ := postTokenReq(t, s, "secret", api.CreatePATRequest{Name: "a", ExpiresIn: "9000h"})
		assert.Equal(t, http.StatusBadRequest, code)
		assert.Equal(t, "expiresIn exceeds tokens.maxLifetime ("+(8760*time.Hour).String()+")\n", body)
	})
	t.Run("max zero keeps the default", func(t *testing.T) {
		s, pg := newLifetimeServer(t, 2160*time.Hour, 0)
		code, body, resp := postTokenReq(t, s, "secret", api.CreatePATRequest{Name: "a"})
		require.Equal(t, http.StatusOK, code, body)
		exp := expiryOf(t, pg, resp.Token)
		require.NotNil(t, exp)
		assert.WithinDuration(t, time.Now().Add(2160*time.Hour), *exp, time.Minute)
	})
	t.Run("max zero only removes the upper bound", func(t *testing.T) {
		s, pg := newLifetimeServer(t, 2160*time.Hour, 0)
		code, body, resp := postTokenReq(t, s, "secret", api.CreatePATRequest{Name: "long", ExpiresIn: "20000h"})
		require.Equal(t, http.StatusOK, code, body)
		exp := expiryOf(t, pg, resp.Token)
		require.NotNil(t, exp, "a PAT always expires")
		assert.WithinDuration(t, time.Now().Add(20000*time.Hour), *exp, time.Minute)
	})
	t.Run("non-positive expiresIn is refused", func(t *testing.T) {
		s, _ := newLifetimeServer(t, 2160*time.Hour, 8760*time.Hour)
		code, _, _ := postTokenReq(t, s, "secret", api.CreatePATRequest{Name: "a", ExpiresIn: "-1h"})
		assert.Equal(t, http.StatusBadRequest, code)
	})
}

func TestAdminSuccessorCapped(t *testing.T) {
	s, pg := newLifetimeServer(t, 2160*time.Hour, 8760*time.Hour)
	s.cfg.AdminEnabled = true
	require.NoError(t, pg.SyncServiceAccounts(context.Background(), []string{testAdminAccount}))
	seedServicePAT(t, pg, authz.BuiltinAdminAccount, "admin-secret")

	code, body, resp := postTokenReq(t, s, "admin-secret", api.CreatePATRequest{Name: "next"})
	require.Equal(t, http.StatusOK, code, body)
	exp := expiryOf(t, pg, resp.Token)
	require.NotNil(t, exp)
	assert.WithinDuration(t, time.Now().Add(720*time.Hour), *exp, time.Minute)
	pat, err := pg.GetPATByHash(context.Background(), HashToken(resp.Token))
	require.NoError(t, err)
	assert.Equal(t, serviceAccountByName(t, pg, authz.BuiltinAdminAccount).ID, pat.AccountID)

	code, body, _ = postTokenReq(t, s, "admin-secret", api.CreatePATRequest{Name: "next2", ExpiresIn: "1000h"})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "expiresIn exceeds the 720h limit for the built-in admin account\n", body)

	code, body, resp = postTokenReq(t, s, "admin-secret", api.CreatePATRequest{Name: "next3", ExpiresIn: "24h"})
	require.Equal(t, http.StatusOK, code, body)
	exp = expiryOf(t, pg, resp.Token)
	require.NotNil(t, exp)
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), *exp, time.Minute)
}

func TestPATCannotTargetOtherAccounts(t *testing.T) {
	s, pg := newLifetimeServer(t, 2160*time.Hour, 8760*time.Hour)
	addPermServicePAT(t, s, pg, "ci", "ci-token", authz.TokenCreate)
	addPermServicePAT(t, s, pg, "deploy", "deploy-token")
	addPermServicePAT(t, s, pg, "tm", "tm-token", authz.TokenCreate, authz.TokenManage)

	code, _, _ := postTokenReq(t, s, "ci-token", api.CreatePATRequest{Name: "x", Account: "deploy"})
	assert.Equal(t, http.StatusForbidden, code)
	code, _, _ = postTokenReq(t, s, "ci-token", api.CreatePATRequest{Name: "x", Account: "admin"})
	assert.Equal(t, http.StatusForbidden, code)

	code, body, _ := postTokenReq(t, s, "tm-token", api.CreatePATRequest{Name: "x", Account: "admin"})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "PATs for the built-in admin account can only be obtained through the initial token or the recovery command\n", body)

	code, body, _ = postTokenReq(t, s, "tm-token", api.CreatePATRequest{Name: "x", Account: "nobody"})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "unknown service account \"nobody\"\n", body)

	// Naming the caller's own account is not "another account".
	code, body, _ = postTokenReq(t, s, "ci-token", api.CreatePATRequest{Name: "own", Account: "ci"})
	assert.Equal(t, http.StatusOK, code, body)
}

func TestServiceAccountTokenPermission(t *testing.T) {
	s, pg := newLifetimeServer(t, 2160*time.Hour, 8760*time.Hour)
	s.cfg.AdminEnabled = true
	addPermServicePAT(t, s, pg, "deploy", "deploy-token")
	// The issuer holds serviceaccount.token and nothing else (no token.create).
	addPermServicePAT(t, s, pg, "issuer", "issuer-token", authz.ServiceAccountToken)

	// A human's PAT and the built-in admin's PAT must stay out of reach.
	human, err := pg.UpsertHumanAccount(context.Background(), "sub-1", "https://issuer.example", "h@example.com", nil)
	require.NoError(t, err)
	humanPAT, err := pg.CreatePAT(context.Background(), store.NewPAT{Name: "human-pat", TokenHash: HashToken("human-token"), AccountID: human.ID})
	require.NoError(t, err)
	adminPAT := seedServicePAT(t, pg, authz.BuiltinAdminAccount, "builtin-admin-token")

	code, body, resp := postTokenReq(t, s, "issuer-token", api.CreatePATRequest{Name: "for-deploy", Account: "deploy"})
	require.Equal(t, http.StatusOK, code, body)
	pat, err := pg.GetPATByHash(context.Background(), HashToken(resp.Token))
	require.NoError(t, err)
	assert.Equal(t, serviceAccountByName(t, pg, "deploy").ID, pat.AccountID)
	assert.Equal(t, "for-deploy", pat.Name)

	// The issuer lists service-account PATs, never a human's or the admin's.
	rec := doAs(t, s, http.MethodGet, "/api/v1/tokens", "issuer-token", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list []api.PATMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	ids := map[string]bool{}
	for _, m := range list {
		ids[m.ID] = true
	}
	assert.True(t, ids[resp.ID], "the service account's PAT is listed")
	assert.False(t, ids[humanPAT.ID], "human PATs stay hidden")
	assert.False(t, ids[adminPAT.ID], "built-in admin PATs stay hidden")

	// It deletes service-account PATs, but human and admin PATs answer 404.
	rec = doAs(t, s, http.MethodDelete, "/api/v1/tokens/"+humanPAT.ID, "issuer-token", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = doAs(t, s, http.MethodDelete, "/api/v1/tokens/"+adminPAT.ID, "issuer-token", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = doAs(t, s, http.MethodDelete, "/api/v1/tokens/"+resp.ID, "issuer-token", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	got, err := pg.GetPATByHash(context.Background(), HashToken(resp.Token))
	assert.True(t, err != nil || got == nil, "the PAT is deleted")

	// An admin sees every PAT, labelled with its account.
	code, body, resp = postTokenReq(t, s, "issuer-token", api.CreatePATRequest{Name: "again", Account: "deploy"})
	require.Equal(t, http.StatusOK, code, body)
	rec = doAs(t, s, http.MethodGet, "/api/v1/tokens", "secret", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	list = nil
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	var found bool
	for _, m := range list {
		if m.ID == resp.ID {
			found = true
			assert.Equal(t, "sa:deploy", m.Account)
		}
	}
	assert.True(t, found)
}

func TestCreateToken_SystemPrefixReserved(t *testing.T) {
	s, _ := newLifetimeServer(t, 2160*time.Hour, 8760*time.Hour)
	code, body, _ := postTokenReq(t, s, "secret", api.CreatePATRequest{Name: "system:x"})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "token name \"system:x\" is reserved\n", body)
}

// A PAT never hands out more than its own ceiling, whichever account it targets.
func TestPATFromPAT_CrossAccountBoundByCeiling(t *testing.T) {
	s, pg := newLifetimeServer(t, 2160*time.Hour, 8760*time.Hour)
	addRoleServicePAT(t, s, pg, "deploy", "deploy-token", "developer")
	issuer := addPermServicePAT(t, s, pg, "issuer", "unused", authz.TokenCreate, authz.ServiceAccountToken, authz.AgentEnroll)
	require.NoError(t, pg.DeletePAT(context.Background(), issuer.ID))
	narrow, err := pg.CreatePAT(context.Background(), store.NewPAT{
		Name: "narrow", TokenHash: HashToken("narrow-token"), AccountID: issuer.AccountID,
		Permissions: []string{"job.read", "token.create", "serviceaccount.token"},
	})
	require.NoError(t, err)
	require.NotNil(t, narrow)

	code, body, resp := postTokenReq(t, s, "narrow-token", api.CreatePATRequest{Name: "for-deploy", Account: "deploy"})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, http.StatusOK, doAs(t, s, http.MethodGet, "/api/v1/jobs", resp.Token, nil).Code)
	trigger := createTokenBody(api.CreatePATRequest{Name: "x"}) // any body: the permission check comes first
	assert.Equal(t, http.StatusForbidden, doAs(t, s, http.MethodPost, "/api/v1/runs", resp.Token, trigger).Code)
}

func TestPATFromPAT_CrossAccountInheritsEnrollLabels(t *testing.T) {
	s, pg := newLifetimeServer(t, 2160*time.Hour, 8760*time.Hour)
	addRoleServicePAT(t, s, pg, "deploy", "deploy-token", "developer")
	issuer := addPermServicePAT(t, s, pg, "issuer", "unused", authz.TokenCreate, authz.ServiceAccountToken, authz.AgentEnroll)
	require.NoError(t, pg.DeletePAT(context.Background(), issuer.ID))
	_, err := pg.CreatePAT(context.Background(), store.NewPAT{
		Name: "bounded", TokenHash: HashToken("bounded-token"), AccountID: issuer.AccountID,
		Permissions: []string{"agent.enroll", "token.create", "serviceaccount.token"}, EnrollLabels: []string{"pool:a"},
	})
	require.NoError(t, err)

	code, body, resp := postTokenReq(t, s, "bounded-token", api.CreatePATRequest{Name: "e", Account: "deploy", Permissions: []string{"agent.enroll"}})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"pool:a"}, resp.EnrollLabels)
}
