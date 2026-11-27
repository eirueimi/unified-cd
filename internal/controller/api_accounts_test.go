package controller

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func listAccountsAs(t *testing.T, s *Server, token, query string) []api.Account {
	t.Helper()
	rec := doAs(t, s, http.MethodGet, "/api/v1/accounts"+query, token, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got []api.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return got
}

func TestAccountsListAndDisable(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{UserMap: map[string]string{"h@example.com": "viewer"}}, "")
	addRoleServicePAT(t, s, pg, testAdminAccount, "secret", "admin")
	addRoleServicePAT(t, s, pg, "dev", "dev-token", "developer")
	human := createOwnedPATForTest(t, pg, store.NewOwnedPAT{Name: "h-pat", TokenHash: HashToken("h-pat-tok"),
		Owner: store.PATOwner{Sub: "h@example.com", Email: "h@example.com", Claims: []string{}}, Role: "viewer"})
	humanAcct := accountOf(t, pg, human)

	// An admin sees every account with its PAT count.
	got := listAccountsAs(t, s, "secret", "")
	byName := map[string]api.Account{}
	for _, a := range got {
		byName[a.Name] = a
	}
	require.Contains(t, byName, authz.BuiltinAdminAccount)
	require.Contains(t, byName, testAdminAccount)
	h := byName["h@example.com"]
	assert.Equal(t, humanAcct.ID, h.ID)
	assert.Equal(t, "human", h.Kind)
	assert.Equal(t, "h@example.com", h.Email)
	assert.False(t, h.Disabled)
	assert.Equal(t, 1, h.PATCount)
	assert.Equal(t, 1, byName[testAdminAccount].PATCount)
	assert.Equal(t, 0, byName[authz.BuiltinAdminAccount].PATCount)

	for _, a := range listAccountsAs(t, s, "secret", "?kind=human") {
		assert.Equal(t, "human", a.Kind)
	}
	svc := listAccountsAs(t, s, "secret", "?kind=service")
	require.NotEmpty(t, svc)
	for _, a := range svc {
		assert.Equal(t, "service", a.Kind)
	}
	assert.Equal(t, http.StatusBadRequest, doAs(t, s, http.MethodGet, "/api/v1/accounts?kind=bogus", "secret", nil).Code)

	// A developer may not list or disable.
	assert.Equal(t, http.StatusForbidden, doAs(t, s, http.MethodGet, "/api/v1/accounts", "dev-token", nil).Code)
	assert.Equal(t, http.StatusForbidden, doAs(t, s, http.MethodPost, "/api/v1/accounts/"+humanAcct.ID+"/disable", "dev-token", nil).Code)

	// Disabling the human refuses its PAT; enabling restores it.
	require.Equal(t, http.StatusOK, doAs(t, s, http.MethodGet, "/api/v1/jobs", "h-pat-tok", nil).Code)
	rec := doAs(t, s, http.MethodPost, "/api/v1/accounts/"+humanAcct.ID+"/disable", "secret", nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	rec = doAs(t, s, http.MethodGet, "/api/v1/jobs", "h-pat-tok", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, forbiddenAccountDisabled, rec.Body.String())
	for _, a := range listAccountsAs(t, s, "secret", "?kind=human") {
		if a.ID == humanAcct.ID {
			assert.True(t, a.Disabled)
		}
	}
	rec = doAs(t, s, http.MethodPost, "/api/v1/accounts/"+humanAcct.ID+"/enable", "secret", nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusOK, doAs(t, s, http.MethodGet, "/api/v1/jobs", "h-pat-tok", nil).Code)

	// Both calls were audited with the account id as the resource.
	logs, err := pg.ListAuditLogs(t.Context(), 10, 0)
	require.NoError(t, err)
	actions := map[string]string{}
	for _, l := range logs {
		actions[l.Action] = l.Resource
	}
	assert.Equal(t, humanAcct.ID, actions["account.disable"])
	assert.Equal(t, humanAcct.ID, actions["account.enable"])

	// Service accounts are managed in the config.
	const want = "service accounts are enabled and disabled in serviceAccounts\n"
	admin := serviceAccountByName(t, pg, authz.BuiltinAdminAccount)
	for _, verb := range []string{"disable", "enable"} {
		rec = doAs(t, s, http.MethodPost, "/api/v1/accounts/"+admin.ID+"/"+verb, "secret", nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, want, rec.Body.String())
	}
	assert.Equal(t, http.StatusNotFound, doAs(t, s, http.MethodPost, "/api/v1/accounts/00000000-0000-0000-0000-000000000000/disable", "secret", nil).Code)
	assert.Equal(t, http.StatusNotFound, doAs(t, s, http.MethodPost, "/api/v1/accounts/not-a-uuid/disable", "secret", nil).Code)
}

// Disabling a human account evicts it from this replica's id_token account
// cache, so the very next request is refused without waiting for the TTL.
func TestDisableEvictsIDTokenAccountCache(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{UserMap: map[string]string{
		"boss@example.com": "admin", "h@example.com": "viewer",
	}}, "")
	now := time.Now()
	s.clock = func() time.Time { return now }

	acct := addHumanAccount(t, pg, "h@example.com", "h@example.com", []string{})
	require.Equal(t, http.StatusOK, doAs(t, s, http.MethodGet, "/api/v1/jobs", bearer("h@example.com"), nil).Code) // fills the cache

	rec := doAs(t, s, http.MethodPost, "/api/v1/accounts/"+acct.ID+"/disable", bearer("boss@example.com"), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	rec = doAs(t, s, http.MethodGet, "/api/v1/jobs", bearer("h@example.com"), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, forbiddenAccountDisabled, rec.Body.String())
}
