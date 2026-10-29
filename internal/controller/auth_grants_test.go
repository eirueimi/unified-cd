package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const teamAPolicy = `
folders:
  team-a:
    groups:
      "g-a": developer
`

const teamABPolicy = `
folders:
  team-a:
    groups:
      "g-a": developer
  team-b:
    groups:
      "g-b": developer
`

func mustOwnedPAT(t *testing.T, st store.Store, token, role string, perms []string, claims []string) *store.PAT {
	t.Helper()
	pat, err := st.CreateOwnedPAT(context.Background(), store.NewOwnedPAT{
		Name: "owned-" + token, TokenHash: HashToken(token), Role: role, Permissions: perms,
		Owner: store.PATOwner{Sub: "sub-" + token, Email: token + "@example.com", Claims: claims},
	})
	require.NoError(t, err)
	return pat
}

func TestServerAuth_BreakGlassHoldsAll25(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, "")
	s.cfg.Token = "bg-token" // break-glass requires the configured bootstrap token
	_, err := pg.UpsertBootstrapPAT(context.Background(), BootstrapPATName, HashToken("bg-token"))
	require.NoError(t, err)

	p, rec := authenticate(t, s, "bg-token", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, p.BreakGlass)
	assert.Equal(t, "admin", p.Role)
	assert.Equal(t, "pat", p.Kind)
	assert.Nil(t, p.Ceiling)
	assert.Empty(t, p.OwnerPATID)
	assert.False(t, p.ClaimsKnown)
	all := authz.All()
	require.Len(t, all, 25)
	for _, perm := range all {
		assert.True(t, p.Grants.Can(perm, ""), string(perm))
		assert.True(t, p.Grants.HeldAnywhere(perm), string(perm))
	}
}

func TestServerAuth_RoleOnlyPATEmptyRoleIsNoRole(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, "")
	_, err := pg.CreatePAT(context.Background(), "empty", HashToken("empty-tok"), "", nil)
	require.NoError(t, err)

	_, rec := authenticate(t, s, "empty-tok", "")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "forbidden: no role\n", rec.Body.String())
}

func TestServerAuth_RoleOnlyPATGrants(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, "")
	_, err := pg.CreatePAT(context.Background(), "v", HashToken("v-tok"), "viewer", nil)
	require.NoError(t, err)

	p, rec := authenticate(t, s, "v-tok", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "viewer", p.Role)
	assert.False(t, p.ClaimsKnown)
	assert.Empty(t, p.Sub)
	assert.True(t, p.Grants.Can(authz.JobRead, "any/job"))
	assert.False(t, p.Grants.Can(authz.RunTrigger, "any/job"))
	_, by := p.Grants.Decide(authz.JobRead, "")
	assert.Equal(t, "role:viewer (pat)", by)
}

func TestServerAuth_OwnerPATEmptyRoleAdmitted(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, teamAPolicy)
	pat := mustOwnedPAT(t, pg, "own1", "", nil, []string{"g-a"})

	p, rec := authenticate(t, s, "own1", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, p.Grants.Can(authz.JobRead, "team-a/x"))
	assert.False(t, p.Grants.Can(authz.JobRead, "other/x"))
	assert.Equal(t, "", p.Role)

	// Owner identity fields (Task 8 consumes these).
	assert.Equal(t, "sub-own1", p.Sub)
	assert.Equal(t, "own1@example.com", p.Email)
	assert.Equal(t, []string{"g-a"}, p.Claims)
	assert.True(t, p.ClaimsKnown)
	assert.Equal(t, pat.ID, p.OwnerPATID)
	assert.False(t, p.BreakGlass)
	assert.Nil(t, p.Ceiling)
	assert.Equal(t, "", p.CeilingRole)
	assert.Nil(t, p.CeilingPerms)
}

func TestServerAuth_OwnerPATCeilingRestricts(t *testing.T) {
	legacy := authz.Legacy{RoleMap: map[string]string{"devs": "developer"}}
	s, pg := newAuthzTestServer(t, legacy, "")
	mustOwnedPAT(t, pg, "cap", "", []string{"job.read"}, []string{"devs"})

	p, rec := authenticate(t, s, "cap", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, p.Grants.Can(authz.JobRead, "x"))
	assert.False(t, p.Grants.Can(authz.RunTrigger, "x"))
	_, by := p.Grants.Decide(authz.JobRead, "x")
	assert.True(t, strings.HasSuffix(by, " & pat-ceiling"), by)
	assert.Equal(t, "", p.Role, "a permission-list ceiling carries no legacy role")
	require.NotNil(t, p.Ceiling)
	assert.True(t, p.Ceiling.Has(authz.JobRead))
	assert.False(t, p.Ceiling.Has(authz.RunTrigger))
	assert.Equal(t, []string{"job.read"}, p.CeilingPerms)
	assert.Equal(t, "", p.CeilingRole)
}

func TestServerAuth_OwnerPATRoleCeilingClampsRole(t *testing.T) {
	legacy := authz.Legacy{RoleMap: map[string]string{"devs": "developer"}}
	s, pg := newAuthzTestServer(t, legacy, "")
	mustOwnedPAT(t, pg, "vcap", "viewer", nil, []string{"devs"})

	p, rec := authenticate(t, s, "vcap", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "viewer", p.Role)
	assert.True(t, p.Grants.Can(authz.JobRead, "x"))
	assert.False(t, p.Grants.Can(authz.RunTrigger, "x"))
	require.NotNil(t, p.Ceiling)
	viewerSet, _ := authz.Bundle("viewer")
	assert.Equal(t, viewerSet, *p.Ceiling)
	assert.Equal(t, "viewer", p.CeilingRole)
	assert.Nil(t, p.CeilingPerms)
}

func TestServerAuth_OwnerPATFollowsCurrentConfig(t *testing.T) {
	pg := store.NewTestPostgres(t)
	mustOwnedPAT(t, pg, "follow", "developer", nil, []string{"owner-group"})

	with := newAuthzTestServerOn(t, pg, authz.Legacy{RoleMap: map[string]string{"owner-group": "developer"}}, "")
	p, rec := authenticate(t, with, "follow", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "developer", p.Role)
	assert.True(t, p.Grants.Can(authz.RunTrigger, "x"))

	without := newAuthzTestServerOn(t, pg, authz.Legacy{}, "")
	p, rec = authenticate(t, without, "follow", "")
	require.Equal(t, http.StatusOK, rec.Code, "an owner PAT is admitted even when nothing matches")
	assert.True(t, p.Grants.IsEmpty())
	assert.Equal(t, "", p.Role)
}

func TestServerAuth_SessionWithClaimsEmptyRoleAdmitted(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, teamAPolicy)
	addSession(t, pg, "sess-claims", "sub-1", "u1@example.com", "", []string{"g-a"})

	p, rec := authenticate(t, s, "", "sess-claims")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "session", p.Kind)
	assert.Equal(t, "", p.Role)
	assert.True(t, p.Grants.Can(authz.RunTrigger, "team-a/x"))
	assert.False(t, p.Grants.Can(authz.RunTrigger, "team-b/x"))
	assert.Equal(t, "sub-1", p.Sub)
	assert.Equal(t, "u1@example.com", p.Email)
	assert.Equal(t, []string{"g-a"}, p.Claims)
	assert.True(t, p.ClaimsKnown)
}

func TestServerAuth_SessionWithoutClaimsEmptyRoleIsNoRole(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, teamAPolicy)
	addSession(t, pg, "sess-old", "sub-2", "u2@example.com", "", nil)

	_, rec := authenticate(t, s, "", "sess-old")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "forbidden: no role\n", rec.Body.String())
}

func TestServerAuth_SessionWithoutClaimsIsRoleOnly(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, teamAPolicy)
	addSession(t, pg, "sess-viewer", "sub-3", "u3@example.com", "viewer", nil)

	p, rec := authenticate(t, s, "", "sess-viewer")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "viewer", p.Role)
	assert.False(t, p.ClaimsKnown)
	assert.True(t, p.Grants.Can(authz.JobRead, "x"))
	_, by := p.Grants.Decide(authz.JobRead, "x")
	assert.Equal(t, "role:viewer (session)", by)
}

func TestServerAuth_BearerDeniedByLegacyButMatchedByFolderIsAdmitted(t *testing.T) {
	s, _ := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, teamAPolicy)

	p, rec := authenticate(t, s, bearer("u@example.com", "g-a"), "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "oidc", p.Kind)
	assert.Equal(t, "", p.Role)
	assert.True(t, p.Grants.Can(authz.JobRead, "team-a/x"))
	assert.False(t, p.Grants.Can(authz.JobRead, "other/x"))
	assert.Equal(t, "u@example.com", p.Sub)
	assert.Equal(t, "u@example.com", p.Email)
}

func TestServerAuth_BearerMatchedByNothingIsNoRole(t *testing.T) {
	s, _ := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, teamAPolicy)

	_, rec := authenticate(t, s, bearer("u@example.com", "unrelated"), "")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "forbidden: no role\n", rec.Body.String())
}

func TestServerAuth_BearerClaimsKnown(t *testing.T) {
	// A roleMap entry makes the groups claim matter: absent claim => unknown.
	s, _ := newAuthzTestServer(t, authz.Legacy{
		RoleMap: map[string]string{"devs": "developer"}, DefaultRole: "viewer",
	}, "")
	p, rec := authenticate(t, s, bearer("a@example.com"), "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, p.ClaimsKnown)

	p, rec = authenticate(t, s, bearer("a@example.com", "devs"), "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, p.ClaimsKnown)
	assert.Equal(t, []string{"devs"}, p.Claims)

	// userMap-only config: the groups claim cannot matter, so absent is still known.
	s2, _ := newAuthzTestServer(t, authz.Legacy{UserMap: map[string]string{"a@example.com": "viewer"}}, "")
	p, rec = authenticate(t, s2, bearer("a@example.com"), "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, p.ClaimsKnown)
	assert.Equal(t, []string{}, p.Claims)
}

func TestIdentityGrants_UserMapDemotionDoesNotSuppressPermissions(t *testing.T) {
	s, _ := newAuthzTestServer(t, authz.Legacy{
		UserMap: map[string]string{"u@example.com": "viewer"},
		RoleMap: map[string]string{"adm": "admin"},
	}, `
folders:
  team-a:
    groups:
      "adm": developer
`)
	role, g, admitted := s.identityGrants([]string{"adm"}, "u@example.com", "u@example.com")
	assert.True(t, admitted)
	assert.Equal(t, "viewer", role)
	assert.True(t, g.Can(authz.JobRead, "other/x"), "viewer bundle")
	assert.False(t, g.Can(authz.RunTrigger, "other/x"), "admin roleMap must stay suppressed")
	assert.True(t, g.Can(authz.RunTrigger, "team-a/x"), "folder grant still applies")
}

func TestResolveRoleSource(t *testing.T) {
	m := RoleMapping{
		RoleMap:     map[string]string{"a": "viewer", "b": "developer"},
		UserMap:     map[string]string{"e@example.com": "admin", "s1": "viewer", "den@example.com": "deny"},
		DefaultRole: "viewer",
	}
	cases := []struct {
		name         string
		values       []string
		email, sub   string
		role, source string
		ok           bool
	}{
		{"email", nil, "e@example.com", "s1", "admin", `userMap["e@example.com"]`, true},
		{"sub", nil, "none@example.com", "s1", "viewer", `userMap["s1"]`, true},
		{"roleMap highest", []string{"a", "b"}, "x@example.com", "x", "developer", `roleMap["b"]`, true},
		{"default", []string{"zz"}, "x@example.com", "x", "viewer", "defaultRole", true},
		{"userMap deny", []string{"b"}, "den@example.com", "x", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			role, source, ok := resolveRoleSource(c.values, c.email, c.sub, m)
			assert.Equal(t, c.role, role)
			assert.Equal(t, c.source, source)
			assert.Equal(t, c.ok, ok)
		})
	}
}

func TestCeilingSet(t *testing.T) {
	assert.Nil(t, ceilingSet("", nil))
	viewer, _ := authz.Bundle("viewer")
	got := ceilingSet("viewer", nil)
	require.NotNil(t, got)
	assert.Equal(t, viewer, *got)

	// A permission list wins over the role.
	got = ceilingSet("admin", []string{"job.read"})
	require.NotNil(t, got)
	assert.Equal(t, authz.Set(0).With(authz.JobRead), *got)

	// An empty (non-nil) list is an empty ceiling, not "no ceiling".
	got = ceilingSet("admin", []string{})
	require.NotNil(t, got)
	assert.True(t, got.IsEmpty())
}

func TestCompleteOIDCLogin_StoresClaimsAndRefreshesPATOwners(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{RoleMap: map[string]string{"g1": "developer"}}, "")
	ctx := context.Background()
	_, err := pg.CreateOwnedPAT(ctx, store.NewOwnedPAT{
		Name: "o", TokenHash: HashToken("o-tok"), Role: "developer",
		Owner: store.PATOwner{Sub: "sub-x", Email: "old@example.com", Claims: []string{"stale"}},
	})
	require.NoError(t, err)

	token, expires, status, msg := s.completeOIDCLogin(ctx, map[string]any{
		"sub": "sub-x", "email": "x@example.com", "groups": []any{"g1"},
	}, "refresh-tok")
	require.Equal(t, 0, status, msg)
	assert.False(t, expires.IsZero())

	sess, err := pg.GetSessionByHash(ctx, HashToken(token))
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, []string{"g1"}, sess.Claims)
	assert.Equal(t, "developer", sess.Role)
	assert.Equal(t, "x@example.com", sess.Email)

	pat, err := pg.GetPATByHash(ctx, HashToken("o-tok"))
	require.NoError(t, err)
	require.NotNil(t, pat.Owner)
	assert.Equal(t, []string{"g1"}, pat.Owner.Claims)
	assert.Equal(t, "x@example.com", pat.Owner.Email)
}

func TestCompleteOIDCLogin_AbsentClaimStoresEmptyNotNull(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{UserMap: map[string]string{"sub-y": "viewer"}}, "")
	token, _, status, msg := s.completeOIDCLogin(context.Background(), map[string]any{"sub": "sub-y"}, "rt")
	require.Equal(t, 0, status, msg)
	sess, err := pg.GetSessionByHash(context.Background(), HashToken(token))
	require.NoError(t, err)
	require.NotNil(t, sess.Claims, "login-time claims must never be stored as NULL")
	assert.Empty(t, sess.Claims)
}

func TestCompleteOIDCLogin_DeniedOnlyWhenNothingMatches(t *testing.T) {
	s, _ := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, teamAPolicy)
	ctx := context.Background()

	_, _, status, msg := s.completeOIDCLogin(ctx, map[string]any{
		"sub": "u", "email": "u@example.com", "groups": []any{"nope"},
	}, "rt")
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "forbidden: no role assigned", msg)

	// Folder-only match is admitted, with an empty global role.
	token, _, status, msg := s.completeOIDCLogin(ctx, map[string]any{
		"sub": "u", "email": "u@example.com", "groups": []any{"g-a"},
	}, "rt")
	require.Equal(t, 0, status, msg)
	sess, err := s.store.GetSessionByHash(ctx, HashToken(token))
	require.NoError(t, err)
	assert.Equal(t, "", sess.Role)
}

func TestRefreshedClaims_UpdatesOnlyWhenClaimPresent(t *testing.T) {
	s, _ := newAuthzTestServer(t, authz.Legacy{}, "")
	ctx := context.Background()

	got, ok := s.refreshedClaims(ctx, bearer("u@example.com", "g1", "g2"))
	assert.True(t, ok)
	assert.Equal(t, []string{"g1", "g2"}, got)

	got, ok = s.refreshedClaims(ctx, bearer("u@example.com"))
	assert.False(t, ok, "claim missing: keep the stored claims")
	assert.Nil(t, got)

	_, ok = s.refreshedClaims(ctx, "")
	assert.False(t, ok, "no id_token in the refresh response")

	_, ok = s.refreshedClaims(ctx, "garbage")
	assert.False(t, ok, "unverifiable token")

	s.verifyIDTokenHook = func(context.Context, string, string) (map[string]any, error) {
		return map[string]any{"groups": []any{}}, nil
	}
	got, ok = s.refreshedClaims(ctx, "x")
	assert.True(t, ok, "a present but empty claim is a real update")
	assert.Equal(t, []string{}, got)
}

func decodeMe(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	return m
}

func TestHandleMe_ReturnsCallerGrantsOnly(t *testing.T) {
	s, _ := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, teamABPolicy)

	rec := doAs(t, s, http.MethodGet, "/api/v1/auth/me", bearer("u@example.com", "g-a"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	m := decodeMe(t, rec.Body.Bytes())
	assert.Equal(t, "oidc", m["kind"])
	assert.Equal(t, "", m["role"])
	assert.Equal(t, "u@example.com", m["sub"])
	assert.Equal(t, "u@example.com", m["email"])
	perms := m["permissions"].(map[string]any)
	assert.Empty(t, perms["global"])
	folders := perms["folders"].(map[string]any)
	assert.Contains(t, folders, "team-a")
	assert.NotContains(t, folders, "team-b")
}

func TestHandleMe_LegacyPATShape(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, "")
	_, err := pg.CreatePAT(context.Background(), "v", HashToken("v-tok"), "viewer", nil)
	require.NoError(t, err)

	rec := doAs(t, s, http.MethodGet, "/api/v1/auth/me", "v-tok", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	m := decodeMe(t, rec.Body.Bytes())
	assert.Equal(t, "viewer", m["role"])
	assert.Equal(t, "pat", m["kind"])
	assert.Equal(t, "v", m["name"])
	assert.NotContains(t, m, "sub")
	assert.NotContains(t, m, "email")
	perms := m["permissions"].(map[string]any)
	viewer, _ := authz.Bundle("viewer")
	want := make([]any, 0)
	for _, n := range viewer.Names() {
		want = append(want, n)
	}
	assert.Equal(t, want, perms["global"])
	assert.Equal(t, map[string]any{}, perms["folders"])
}

func TestHandleMe_Unauthenticated(t *testing.T) {
	s, _ := newAuthzTestServer(t, authz.Legacy{}, "")
	rec := doAs(t, s, http.MethodGet, "/api/v1/auth/me", "", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestServerAuth_NameAloneIsNotBreakGlass(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, "")
	s.cfg.Token = "real-bootstrap"
	ctx := context.Background()
	// An attacker-created developer PAT that merely carries the reserved name.
	_, err := pg.CreatePAT(ctx, BootstrapPATName, HashToken("attacker-tok"), "developer", nil)
	require.NoError(t, err)

	p, rec := authenticate(t, s, "attacker-tok", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, p.BreakGlass)
	assert.Equal(t, "developer", p.Role)
	assert.True(t, p.Grants.Can(authz.RunTrigger, "x"))
	assert.False(t, p.Grants.Can(authz.AuthzRead, ""))
	assert.False(t, p.Grants.Can(authz.SecretWrite, ""))

	// The real bootstrap token is break-glass.
	_, err = pg.UpsertBootstrapPAT(ctx, "other-row", HashToken("real-bootstrap"))
	require.NoError(t, err)
	p, rec = authenticate(t, s, "real-bootstrap", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, p.BreakGlass)
	assert.Equal(t, "admin", p.Role)
	assert.True(t, p.Grants.Can(authz.AuthzRead, ""))
}

func TestServerAuth_NoServerOrNoTokenMeansNoBreakGlass(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, "")
	_, err := pg.UpsertBootstrapPAT(context.Background(), BootstrapPATName, HashToken("bg-token"))
	require.NoError(t, err)

	// No configured bootstrap token: the row is an ordinary PAT (role admin
	// from the row, but not BreakGlass).
	p, rec := authenticate(t, s, "bg-token", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, p.BreakGlass)

	// ServerAuth(st, nil): no break-glass at all.
	var got Principal
	h := ServerAuth(pg, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = principalFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", bearerPrefix+"bg-token")
	h.ServeHTTP(httptest.NewRecorder(), req)
	assert.False(t, got.BreakGlass)
}

func TestCreateToken_ReservedNamesRefused(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, "")
	_, err := pg.CreatePAT(context.Background(), "dev", HashToken("dev-tok"), "developer", nil)
	require.NoError(t, err)
	for _, name := range []string{"env:UNIFIED_TOKEN", "env:x"} {
		body, _ := json.Marshal(map[string]string{"name": name})
		rec := doAs(t, s, http.MethodPost, "/api/v1/tokens", "dev-tok", body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, name)
		assert.Equal(t, fmt.Sprintf("token name %q is reserved\n", name), rec.Body.String())
	}
}

func TestCompleteOIDCLogin_DeniedLoginStillRefreshesPATOwnerSnapshot(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{RoleMap: map[string]string{"g1": "developer"}}, "")
	ctx := context.Background()
	mustOwnedPAT(t, pg, "stale", "developer", nil, []string{"g1"})

	p, rec := authenticate(t, s, "stale", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, p.Grants.Can(authz.RunTrigger, "x"))

	_, _, status, msg := s.completeOIDCLogin(ctx, map[string]any{
		"sub": "sub-stale", "email": "stale@example.com", "groups": []any{"unrelated"},
	}, "rt")
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "forbidden: no role assigned", msg)

	p, rec = authenticate(t, s, "stale", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, p.Grants.IsEmpty(), "PAT must resolve against the refreshed claims")
}
