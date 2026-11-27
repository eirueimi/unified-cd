package controller

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/eirueimi/unified-cd/internal/store/storetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const forbiddenAccountDisabled = "forbidden: account disabled\n"

// A disabled human account is refused on every credential: session cookie,
// PAT and bearer id_token. The id_token path trusts a cached account row for a
// minute, so the test advances the server clock instead of sleeping.
func TestDisabledHumanRefusedEverywhere(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{UserMap: map[string]string{"h@example.com": "admin"}}, "")
	now := time.Now()
	s.clock = func() time.Time { return now }

	acct := addHumanAccount(t, pg, "h@example.com", "h@example.com", []string{})
	addSession(t, pg, "sess-h", "h@example.com", "h@example.com", "admin", []string{})
	createOwnedPATForTest(t, pg, store.NewOwnedPAT{Name: "h-pat", TokenHash: HashToken("h-pat-tok"),
		Owner: store.PATOwner{Sub: "h@example.com", Email: "h@example.com", Claims: []string{}}})
	idToken := bearer("h@example.com")

	// Everything works while the account is enabled; the id_token request
	// fills the account cache.
	assert.Equal(t, http.StatusOK, doAs(t, s, http.MethodGet, "/api/v1/jobs", idToken, nil).Code)
	assert.Equal(t, http.StatusOK, doAs(t, s, http.MethodGet, "/api/v1/jobs", "h-pat-tok", nil).Code)
	assert.Equal(t, http.StatusOK, doWithCookie(t, s, http.MethodGet, "/api/v1/jobs", "sess-h", nil).Code)

	require.NoError(t, pg.SetAccountDisabled(t.Context(), acct.ID, true))

	rec := doAs(t, s, http.MethodGet, "/api/v1/jobs", "h-pat-tok", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, forbiddenAccountDisabled, rec.Body.String())

	rec = doWithCookie(t, s, http.MethodGet, "/api/v1/jobs", "sess-h", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, forbiddenAccountDisabled, rec.Body.String())

	// The id_token path still holds the cached (enabled) row ...
	assert.Equal(t, http.StatusOK, doAs(t, s, http.MethodGet, "/api/v1/jobs", idToken, nil).Code)
	// ... until the cache entry is a minute old.
	now = now.Add(idTokenAccountTTL + time.Second)
	rec = doAs(t, s, http.MethodGet, "/api/v1/jobs", idToken, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, forbiddenAccountDisabled, rec.Body.String())
}

// The id_token path writes the account at most once a minute per sub.
func TestIDTokenAccountUpsertedOncePerMinute(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{UserMap: map[string]string{"u@example.com": "viewer"}}, "")
	now := time.Now()
	s.clock = func() time.Time { return now }

	require.Equal(t, http.StatusOK, doAs(t, s, http.MethodGet, "/api/v1/jobs", bearer("u@example.com"), nil).Code)
	first, err := pg.GetHumanAccountBySub(t.Context(), "u@example.com")
	require.NoError(t, err)
	require.NotNil(t, first)
	require.NotNil(t, first.LastSeenAt)

	now = now.Add(30 * time.Second)
	require.Equal(t, http.StatusOK, doAs(t, s, http.MethodGet, "/api/v1/jobs", bearer("u@example.com"), nil).Code)
	again, err := pg.GetHumanAccountBySub(t.Context(), "u@example.com")
	require.NoError(t, err)
	assert.True(t, again.LastSeenAt.Equal(*first.LastSeenAt), "no second write within a minute")

	now = now.Add(time.Minute)
	require.Equal(t, http.StatusOK, doAs(t, s, http.MethodGet, "/api/v1/jobs", bearer("u@example.com"), nil).Code)
	later, err := pg.GetHumanAccountBySub(t.Context(), "u@example.com")
	require.NoError(t, err)
	assert.True(t, later.LastSeenAt.After(*first.LastSeenAt), "refreshed after the cache expired")
}

// A service account that leaves the config is disabled by the startup sync and
// its PATs stop working; adding it back re-enables the very same PAT.
func TestServiceAccountRemovedFromConfig(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	withCI := Config{ServiceAccountGrants: map[string]authz.Grants{"ci": authz.RoleGrants("viewer", "role:viewer")}}
	syncTestServiceAccounts(t, pg, withCI)
	seedServicePAT(t, pg, "ci", "ci-token")

	s := NewServer(withCI, pg)
	t.Cleanup(s.Close)
	assert.Equal(t, http.StatusOK, doAs(t, s, http.MethodGet, "/api/v1/jobs", "ci-token", nil).Code)

	without := Config{}
	syncTestServiceAccounts(t, pg, without)
	s2 := NewServer(without, pg)
	t.Cleanup(s2.Close)
	rec := doAs(t, s2, http.MethodGet, "/api/v1/jobs", "ci-token", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, forbiddenAccountDisabled, rec.Body.String())

	// Not yet synced but already gone from the config: still refused.
	syncTestServiceAccounts(t, pg, withCI)
	s3 := NewServer(without, pg)
	t.Cleanup(s3.Close)
	rec = doAs(t, s3, http.MethodGet, "/api/v1/jobs", "ci-token", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, forbiddenAccountDisabled, rec.Body.String())

	s4 := NewServer(withCI, pg)
	t.Cleanup(s4.Close)
	assert.Equal(t, http.StatusOK, doAs(t, s4, http.MethodGet, "/api/v1/jobs", "ci-token", nil).Code)
}

// The built-in admin account authenticates only while the config enables it or
// a recovery lease is running.
func TestAdminAccountDisabled(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	require.NoError(t, pg.SyncServiceAccounts(t.Context(), nil))
	admin := serviceAccountByName(t, pg, authz.BuiltinAdminAccount)
	_, err := pg.CreateAccountPAT(t.Context(), store.NewPAT{Name: "adm", TokenHash: HashToken("adm-token"), AccountID: admin.ID})
	require.NoError(t, err)

	enabled := NewServer(Config{AdminEnabled: true}, pg)
	t.Cleanup(enabled.Close)
	assert.Equal(t, http.StatusOK, doAs(t, enabled, http.MethodGet, "/api/v1/jobs", "adm-token", nil).Code)

	disabled := NewServer(Config{AdminEnabled: false}, pg)
	t.Cleanup(disabled.Close)
	rec := doAs(t, disabled, http.MethodGet, "/api/v1/jobs", "adm-token", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, forbiddenAccountDisabled, rec.Body.String())

	// A running lease enables it again.
	require.NoError(t, pg.SetAdminEnabledUntil(t.Context(), time.Now().Add(time.Hour)))
	leased := NewServer(Config{AdminEnabled: false}, pg)
	t.Cleanup(leased.Close)
	assert.Equal(t, http.StatusOK, doAs(t, leased, http.MethodGet, "/api/v1/jobs", "adm-token", nil).Code)

	// The lease read is cached for 10 seconds and then re-read; an expired
	// lease no longer enables the account.
	now := time.Now()
	leased.clock = func() time.Time { return now }
	require.NoError(t, pg.SetAdminEnabledUntil(t.Context(), now.Add(time.Minute)))
	now = now.Add(2 * time.Minute)
	assert.Equal(t, http.StatusForbidden, doAs(t, leased, http.MethodGet, "/api/v1/jobs", "adm-token", nil).Code)
}

// A PAT row with no account (a pre-accounts row) is refused.
func TestOwnerlessPATRefused(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	require.NoError(t, storetest.Exec(t.Context(), pg,
		`INSERT INTO pats (name, token_hash, role, account_id) VALUES ('legacy', $1, 'admin', NULL)`, HashToken("legacy-token")))
	s := NewServer(Config{AdminEnabled: true}, pg)
	t.Cleanup(s.Close)

	rec := doAs(t, s, http.MethodGet, "/api/v1/jobs", "legacy-token", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, forbiddenAccountDisabled, rec.Body.String())
}

// A person the policy refuses leaves no account row behind.
func TestRefusedLoginWritesNoAccount(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, teamAPolicy)

	status, msg := s.admitOIDCClaims(t.Context(), map[string]any{
		"sub": "nobody", "email": "nobody@example.com", "groups": []any{"unrelated"},
	})
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "forbidden: no role assigned", msg)

	// The same through the bearer id_token path.
	rec := doAs(t, s, http.MethodGet, "/api/v1/jobs", bearer("nobody@example.com", "unrelated"), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "forbidden: no role\n", rec.Body.String())

	accts, err := pg.ListAccounts(t.Context())
	require.NoError(t, err)
	for _, a := range accts {
		assert.NotEqual(t, store.AccountKindHuman, a.Kind, "no human account for a refused login")
	}

	// An admitted login does write the account.
	status, msg = s.admitOIDCClaims(t.Context(), map[string]any{
		"sub": "ok", "email": "ok@example.com", "groups": []any{"g-a"},
	})
	require.Equal(t, 0, status, msg)
	acct, err := pg.GetHumanAccountBySub(t.Context(), "ok")
	require.NoError(t, err)
	require.NotNil(t, acct)
	assert.Equal(t, "ok@example.com", acct.Email)
	assert.Equal(t, []string{"g-a"}, acct.Claims)
}

// A disabled human is refused at login with a clear message, and the login
// does not clear the flag.
func TestDisabledHumanLoginRefused(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{UserMap: map[string]string{"d@example.com": "viewer"}}, "")
	acct := addHumanAccount(t, pg, "d@example.com", "d@example.com", []string{})
	require.NoError(t, pg.SetAccountDisabled(t.Context(), acct.ID, true))

	status, msg := s.admitOIDCClaims(t.Context(), map[string]any{"sub": "d@example.com", "email": "d@example.com"})
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "forbidden: account disabled", msg)
	got, err := pg.GetAccount(t.Context(), acct.ID)
	require.NoError(t, err)
	assert.NotNil(t, got.DisabledAt)
}

// A service-account PAT's audit rows name the account.
func TestServiceAccountAuditDetail(t *testing.T) {
	s, pg := newTestServer(t)
	addRoleServicePAT(t, s, pg, "ci", "ci-token", "admin")

	rec := doAs(t, s, http.MethodDelete, "/api/v1/vars/probe", "ci-token", nil)
	require.NotEqual(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	require.NotEqual(t, http.StatusForbidden, rec.Code, rec.Body.String())

	logs, err := pg.ListAuditLogs(t.Context(), 1, 0)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	assert.Equal(t, "vars.delete", logs[0].Action)
	assert.Equal(t, "ci", logs[0].Actor)
	var d map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Details, &d))
	assert.Equal(t, "sa:ci", d["account"])
	pat, err := pg.GetPATByHash(t.Context(), HashToken("ci-token"))
	require.NoError(t, err)
	assert.Equal(t, "pat:"+pat.ID, logs[0].ActorID)
}

// An account whose grants vanish from the config (a service account without
// permissions) is refused with "no role".
func TestServiceAccountWithoutGrantsIsNoRole(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	cfg := Config{ServiceAccountGrants: map[string]authz.Grants{"empty": {}}}
	syncTestServiceAccounts(t, pg, cfg)
	seedServicePAT(t, pg, "empty", "empty-token")
	s := NewServer(cfg, pg)
	t.Cleanup(s.Close)

	rec := doAs(t, s, http.MethodGet, "/api/v1/jobs", "empty-token", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "forbidden: no role\n", rec.Body.String())
}

// A PAT never exceeds its account: the ceiling only narrows.
func TestServiceAccountPATCeiling(t *testing.T) {
	s, pg := newTestServer(t)
	addRoleServicePAT(t, s, pg, "dev", "dev-token", "developer")
	acct := serviceAccountByName(t, pg, "dev")
	_, err := pg.CreateAccountPAT(t.Context(), store.NewPAT{Name: "ro", TokenHash: HashToken("ro-token"), AccountID: acct.ID,
		Permissions: []string{"job.read", "secret.write"}})
	require.NoError(t, err)

	p, rec := authenticate(t, s, "ro-token", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, p.Grants.Can(authz.JobRead, "x"))
	assert.False(t, p.Grants.Can(authz.RunTrigger, "x"), "outside the ceiling")
	assert.False(t, p.Grants.Can(authz.SecretWrite, ""), "inside the ceiling but not held by the account")
}
