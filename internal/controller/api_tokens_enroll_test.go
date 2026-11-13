package controller

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/eirueimi/unified-cd/internal/store/storetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const enrollAdminPolicy = `
global:
  groups:
    "g-mgr": [token.create, agent.manage]
    "g-none": [token.create, job.read]
    "g-enr": [token.create, agent.enroll]
`

func postToken(t *testing.T, s *Server, token string, req api.CreatePATRequest) (int, string, api.CreatePATResponse) {
	t.Helper()
	body, _ := json.Marshal(req)
	rec := doAs(t, s, http.MethodPost, "/api/v1/tokens", token, body)
	var resp api.CreatePATResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec.Code, rec.Body.String(), resp
}

// breakGlassServer is a server whose UNIFIED_TOKEN ("secret") is synced as the
// break-glass PAT, as at startup.
func breakGlassServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	pg := storetest.NewTestPostgres(t)
	s := NewServer(Config{Token: "secret"}, pg)
	t.Cleanup(s.Close)
	_, err := pg.CreatePAT(t.Context(), BootstrapPATName, HashToken("secret"), "admin", nil, "")
	require.NoError(t, err)
	return s, pg
}

func TestCreateToken_OwnerEnrollPATWithLabels(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, enrollAdminPolicy)
	code, body, resp := postToken(t, s, bearer("m@example.com", "g-mgr"),
		api.CreatePATRequest{Name: "prov", Permissions: []string{"agent.enroll"}, EnrollLabels: []string{"kind:linux", "pool:build"}})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"kind:linux", "pool:build"}, resp.EnrollLabels)
	assert.Equal(t, []string{"agent.enroll"}, resp.Permissions)
	pat, err := pg.GetPATByHash(t.Context(), HashToken(resp.Token))
	require.NoError(t, err)
	assert.Equal(t, []string{"kind:linux", "pool:build"}, pat.EnrollLabels)
	require.NotNil(t, pat.Owner)
}

func TestCreateToken_BreakGlassMintsEnrollPAT(t *testing.T) {
	s, pg := breakGlassServer(t)
	code, body, resp := postToken(t, s, "secret",
		api.CreatePATRequest{Name: "prov", Permissions: []string{"agent.enroll"}, EnrollLabels: []string{"kind:linux"}})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"kind:linux"}, resp.EnrollLabels)
	pat, err := pg.GetPATByHash(t.Context(), HashToken(resp.Token))
	require.NoError(t, err)
	assert.Nil(t, pat.Owner)
	assert.Equal(t, "", pat.Role)
	assert.Equal(t, []string{"agent.enroll"}, pat.Permissions)

	// Without labels too.
	code, body, _ = postToken(t, s, "secret", api.CreatePATRequest{Name: "prov2", Permissions: []string{"agent.enroll"}})
	require.Equal(t, http.StatusOK, code, body)
}

func TestCreateToken_RoleOnlyCreatorOtherPermissionsStill400(t *testing.T) {
	s, _ := breakGlassServer(t)
	for _, req := range []api.CreatePATRequest{
		{Name: "x", Permissions: []string{"agent.enroll", "job.read"}},
		{Name: "x", Permissions: []string{"job.read"}},
		{Name: "x", Role: "viewer", Permissions: []string{"agent.enroll"}},
	} {
		code, body, _ := postToken(t, s, "secret", req)
		assert.Equal(t, http.StatusBadRequest, code)
		assert.Equal(t, "cannot grant permissions: this token has no owner; use --role\n", body)
	}
}

func TestCreateToken_EnrollLabelValidation(t *testing.T) {
	s, _ := breakGlassServer(t)
	cases := []struct {
		name string
		req  api.CreatePATRequest
		want string
	}{
		{"without agent.enroll", api.CreatePATRequest{Name: "x", Role: "viewer", EnrollLabels: []string{"a"}}, "enrollLabels requires the agent.enroll permission\n"},
		{"with other permission", api.CreatePATRequest{Name: "x", Permissions: []string{"job.read"}, EnrollLabels: []string{"a"}}, "enrollLabels requires the agent.enroll permission\n"},
		{"blank", api.CreatePATRequest{Name: "x", Permissions: []string{"agent.enroll"}, EnrollLabels: []string{"a", " "}}, "enrollLabels must not contain empty entries\n"},
		{"duplicate", api.CreatePATRequest{Name: "x", Permissions: []string{"agent.enroll"}, EnrollLabels: []string{"a", "a"}}, "enrollLabels must not contain duplicates\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body, _ := postToken(t, s, "secret", c.req)
			assert.Equal(t, http.StatusBadRequest, code)
			assert.Equal(t, c.want, body)
		})
	}
}

func TestCreateToken_EnrollRequiresHoldingEnrollOrManage(t *testing.T) {
	s, _ := newAuthzTestServer(t, authz.Legacy{}, enrollAdminPolicy)
	code, body, _ := postToken(t, s, bearer("n@example.com", "g-none"),
		api.CreatePATRequest{Name: "prov", Permissions: []string{"agent.enroll"}})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "cannot grant permission \"agent.enroll\": you do not hold it\n", body)
}

func TestCreateToken_EnrollPATFromPATLabelSubset(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, enrollAdminPolicy)
	// Owner parent holding agent.manage, bounded to kind:linux.
	_, _, parent := postToken(t, s, bearer("m@example.com", "g-mgr"),
		api.CreatePATRequest{Name: "parent", Permissions: []string{"agent.enroll", "token.create"}, EnrollLabels: []string{"kind:linux"}})
	require.NotEmpty(t, parent.Token)

	code, body, _ := postToken(t, s, parent.Token,
		api.CreatePATRequest{Name: "child", Permissions: []string{"agent.enroll"}, EnrollLabels: []string{"pool:build"}})
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, "enrollLabels must be a subset of the creating token's enrollLabels\n", body)

	code, body, ok := postToken(t, s, parent.Token,
		api.CreatePATRequest{Name: "child", Permissions: []string{"agent.enroll"}, EnrollLabels: []string{"kind:linux"}})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"kind:linux"}, ok.EnrollLabels)

	// No labels requested: the child inherits the parent's bound, never unbounded.
	code, body, inh := postToken(t, s, parent.Token,
		api.CreatePATRequest{Name: "child2", Permissions: []string{"agent.enroll"}})
	require.Equal(t, http.StatusOK, code, body)
	pat, err := pg.GetPATByHash(t.Context(), HashToken(inh.Token))
	require.NoError(t, err)
	assert.Equal(t, []string{"kind:linux"}, pat.EnrollLabels)
}

func TestListTokens_ShowsEnrollLabels(t *testing.T) {
	s, _ := breakGlassServer(t)
	code, body, _ := postToken(t, s, "secret", api.CreatePATRequest{Name: "prov", Permissions: []string{"agent.enroll"}, EnrollLabels: []string{"kind:linux"}})
	require.Equal(t, http.StatusOK, code, body)
	rec := doAs(t, s, http.MethodGet, "/api/v1/tokens", "secret", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list []api.PATMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	var found bool
	for _, m := range list {
		if m.Name == "prov" {
			found = true
			assert.Equal(t, []string{"kind:linux"}, m.EnrollLabels)
		}
	}
	assert.True(t, found)
}

func TestPatPrincipal_EnrollPAT(t *testing.T) {
	s, pg := breakGlassServer(t)
	_, _, resp := postToken(t, s, "secret", api.CreatePATRequest{Name: "prov", Permissions: []string{"agent.enroll"}, EnrollLabels: []string{"kind:linux"}})
	pat, err := pg.GetPATByHash(t.Context(), HashToken(resp.Token))
	require.NoError(t, err)
	p, ok := s.patPrincipal(pat, HashToken(resp.Token))
	require.True(t, ok)
	assert.True(t, p.Grants.Can(authz.AgentEnroll, ""))
	assert.False(t, p.Grants.Can(authz.AgentRead, ""))
	assert.False(t, p.Grants.Can(authz.AgentManage, ""))
	assert.Equal(t, []string{"kind:linux"}, p.EnrollLabels)
	assert.Equal(t, []string{"pat:prov"}, p.Grants.Explain(authz.AgentEnroll, ""))
}

func TestPatPrincipal_OwnerPATCopiesEnrollLabels(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, enrollAdminPolicy)
	_, _, resp := postToken(t, s, bearer("m@example.com", "g-mgr"),
		api.CreatePATRequest{Name: "prov", Permissions: []string{"agent.enroll"}, EnrollLabels: []string{"kind:linux"}})
	pat, err := pg.GetPATByHash(t.Context(), HashToken(resp.Token))
	require.NoError(t, err)
	p, ok := s.patPrincipal(pat, HashToken(resp.Token))
	require.True(t, ok)
	assert.Equal(t, []string{"kind:linux"}, p.EnrollLabels)
	// The owner holds agent.manage, which stands in for agent.enroll.
	assert.True(t, p.Grants.Can(authz.AgentEnroll, ""))
}

// A parent PAT whose ceiling holds agent.manage (not agent.enroll) can mint an
// enrollment child that actually works.
func TestCreateToken_EnrollChildOfManageCeilingParent(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, enrollAdminPolicy)
	_, _, parent := postToken(t, s, bearer("m@example.com", "g-mgr"),
		api.CreatePATRequest{Name: "parent", Permissions: []string{"agent.manage", "token.create"}})
	require.NotEmpty(t, parent.Token)

	code, body, child := postToken(t, s, parent.Token,
		api.CreatePATRequest{Name: "child", Permissions: []string{"agent.enroll"}, EnrollLabels: []string{"kind:linux"}})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"agent.enroll"}, child.Permissions)
	assert.Equal(t, []string{"kind:linux"}, child.EnrollLabels)

	pat, err := pg.GetPATByHash(t.Context(), HashToken(child.Token))
	require.NoError(t, err)
	p, ok := s.patPrincipal(pat, HashToken(child.Token))
	require.True(t, ok)
	assert.True(t, p.Grants.Can(authz.AgentEnroll, ""))
	assert.Equal(t, []string{"kind:linux"}, p.EnrollLabels)
}

// An owner holding neither agent.manage nor agent.enroll gets no agent.enroll
// from a PAT, even one whose ceiling lists it.
func TestPatPrincipal_OwnerWithoutManageOrEnrollGetsNoEnroll(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, enrollAdminPolicy)
	_, err := pg.CreateOwnedPAT(t.Context(), store.NewOwnedPAT{
		Name: "weak", TokenHash: HashToken("exc_weak"),
		Owner:       store.PATOwner{Sub: "n@example.com", Email: "n@example.com", Claims: []string{"g-none"}},
		Permissions: []string{"agent.enroll", "job.read"},
	})
	require.NoError(t, err)
	pat, err := pg.GetPATByHash(t.Context(), HashToken("exc_weak"))
	require.NoError(t, err)
	p, ok := s.patPrincipal(pat, HashToken("exc_weak"))
	require.True(t, ok)
	assert.False(t, p.Grants.Can(authz.AgentEnroll, ""))
	assert.True(t, p.Grants.Can(authz.JobRead, ""))
}

// A creator without agent.manage can only grant labels inside its own bound;
// a missing bound means no labels at all.
func TestCreateToken_UnboundedEnrollParentCannotGrantLabels(t *testing.T) {
	s, _ := newAuthzTestServer(t, authz.Legacy{}, enrollAdminPolicy)
	_, _, parent := postToken(t, s, bearer("m@example.com", "g-mgr"),
		api.CreatePATRequest{Name: "parent", Permissions: []string{"agent.enroll", "token.create"}})
	require.NotEmpty(t, parent.Token)

	rec := enrollmentRequest(t, s, parent.Token, api.CreateAgentEnrollmentRequest{Labels: []string{"pool:gpu"}})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	code, body, _ := postToken(t, s, parent.Token,
		api.CreatePATRequest{Name: "child", Permissions: []string{"agent.enroll"}, EnrollLabels: []string{"pool:gpu"}})
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, "enrollLabels must be a subset of the creating token's enrollLabels\n", body)

	// Without labels the child is still allowed (it simply carries none).
	code, body, _ = postToken(t, s, parent.Token,
		api.CreatePATRequest{Name: "child2", Permissions: []string{"agent.enroll"}})
	require.Equal(t, http.StatusOK, code, body)
}

func TestCreateToken_SessionWithEnrollButNotManageCannotGrantLabels(t *testing.T) {
	s, _ := newAuthzTestServer(t, authz.Legacy{}, enrollAdminPolicy)
	code, body, _ := postToken(t, s, bearer("e@example.com", "g-enr"),
		api.CreatePATRequest{Name: "prov", Permissions: []string{"agent.enroll"}, EnrollLabels: []string{"pool:gpu"}})
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, "enrollLabels must be a subset of the creating token's enrollLabels\n", body)
}

func TestCreateToken_ManageHolderMayGrantAnyLabels(t *testing.T) {
	s, _ := newAuthzTestServer(t, authz.Legacy{}, enrollAdminPolicy)
	code, body, resp := postToken(t, s, bearer("m@example.com", "g-mgr"),
		api.CreatePATRequest{Name: "prov", Permissions: []string{"agent.enroll"}, EnrollLabels: []string{"pool:gpu"}})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"pool:gpu"}, resp.EnrollLabels)
}
