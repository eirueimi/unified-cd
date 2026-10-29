package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const authzViewPolicy = `
global:
  users:
    "reader@example.com": [authz.read]
  groups:
    "my-org:auditors": [viewer, audit.read]
folders:
  team-a:
    groups:
      "my-org:team-a": developer
  team-a/prod:
    groups:
      "my-org:sre": [job.read, run.approve]
  team-b:
    groups:
      "my-org:team-b": developer
    users:
      contractor@example.com: [job.read, run.trigger]
`

const missingAuthzRead = "forbidden: missing permission \"authz.read\"\n"

func authzViewServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, pg := newAuthzTestServer(t, authz.Legacy{
		RoleMap:     map[string]string{"my-org:platform": "admin"},
		UserMap:     map[string]string{"oncall-lead@example.com": "admin"},
		DefaultRole: "deny",
	}, authzViewPolicy)
	makeRolePAT(t, pg, "tok-admin", "admin")
	makeRolePAT(t, pg, "tok-viewer", "viewer")
	makeRolePAT(t, pg, "tok-developer", "developer")
	return s, "tok-admin"
}

func explain(t *testing.T, s *Server, token string, req api.AuthzExplainRequest) (api.AuthzExplainResponse, *httptest.ResponseRecorder) {
	t.Helper()
	rec := bodyPost(t, s, "/api/v1/authz/explain", token, req)
	var resp api.AuthzExplainResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return resp, rec
}

func grantedBy(resp api.AuthzExplainResponse, perm string) []string {
	for _, p := range resp.Permissions {
		if p.Permission == perm {
			return p.GrantedBy
		}
	}
	return nil
}

// strs returns a non-nil list, so strs() is an explicitly empty "groups".
func strs(v ...string) *[]string {
	if v == nil {
		v = []string{}
	}
	return &v
}

func TestAuthzRead_AdminOnlyAmongBundles(t *testing.T) {
	s, _ := authzViewServer(t)
	for _, tok := range []string{"tok-viewer", "tok-developer"} {
		rec := doAs(t, s, http.MethodGet, "/api/v1/authz/policy", tok, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, tok)
		assert.Equal(t, missingAuthzRead, rec.Body.String(), tok)

		_, rec = explain(t, s, tok, api.AuthzExplainRequest{User: "a@example.com"})
		assert.Equal(t, http.StatusForbidden, rec.Code, tok)
		assert.Equal(t, missingAuthzRead, rec.Body.String(), tok)
	}
	assert.Equal(t, http.StatusOK, doAs(t, s, http.MethodGet, "/api/v1/authz/policy", "tok-admin", nil).Code)
	_, rec := explain(t, s, "tok-admin", api.AuthzExplainRequest{User: "a@example.com"})
	assert.Equal(t, http.StatusOK, rec.Code)

	// permissions.global grants authz.read on its own.
	assert.Equal(t, http.StatusOK, doAs(t, s, http.MethodGet, "/api/v1/authz/policy", bearer("reader@example.com"), nil).Code)
	_, rec = explain(t, s, bearer("reader@example.com"), api.AuthzExplainRequest{User: "a@example.com"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthzRead_BreakGlass(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{}, "")
	s.cfg.Token = "bg-token"
	_, err := pg.UpsertBootstrapPAT(t.Context(), BootstrapPATName, HashToken("bg-token"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, doAs(t, s, http.MethodGet, "/api/v1/authz/policy", "bg-token", nil).Code)
	_, rec := explain(t, s, "bg-token", api.AuthzExplainRequest{User: "a@example.com"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthzPolicy_ListsResolvedPolicy(t *testing.T) {
	s, tok := authzViewServer(t)
	rec := doAs(t, s, http.MethodGet, "/api/v1/authz/policy", tok, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var p api.AuthzPolicy
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	assert.True(t, p.OIDC)
	assert.Equal(t, "groups", p.RolesClaim)
	assert.Len(t, p.Bundles["admin"], 25)
	require.Len(t, p.Global, 2)
	require.Len(t, p.Folders, 3)
	assert.Equal(t, []string{"team-a", "team-a/prod", "team-b"}, []string{p.Folders[0].Folder, p.Folders[1].Folder, p.Folders[2].Folder})
	require.Len(t, p.Legacy.RoleMap, 1)
	assert.Equal(t, "admin", p.Legacy.RoleMap[0].Role)
	assert.Equal(t, "deny", p.Legacy.DefaultRole.Role)
}

func TestAuthzPolicy_NoOIDC(t *testing.T) {
	s, pg := newTestServer(t)
	makeRolePAT(t, pg, "tok-admin", "admin")
	rec := doAs(t, s, http.MethodGet, "/api/v1/authz/policy", "tok-admin", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var p api.AuthzPolicy
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	assert.False(t, p.OIDC)
	assert.Len(t, p.Bundles, 3)
	assert.Empty(t, p.Legacy.RoleMap)
	assert.Empty(t, p.Legacy.UserMap)
	assert.Empty(t, p.Global)
	assert.Empty(t, p.Folders)
	assert.Contains(t, rec.Body.String(), `"global":[]`)
	assert.Contains(t, rec.Body.String(), `"folders":[]`)
}

// TestAuthzEndpoints_ResponsesCarryNoSecrets puts distinctive secret values
// into the server configuration and checks that neither endpoint echoes them.
func TestAuthzEndpoints_ResponsesCarryNoSecrets(t *testing.T) {
	const (
		clientSecret = "s3cr3t-client-secret-9f2c"
		bootToken    = "tok-break-glass-7a1d"
		deviceID     = "device-client-id-4e8b"
	)
	s, tok := authzViewServer(t)
	s.cfg.Token = bootToken
	s.oidcCfg.ClientSecret = clientSecret
	s.oidcCfg.DeviceClientID = deviceID

	bodies := []string{doAs(t, s, http.MethodGet, "/api/v1/authz/policy", tok, nil).Body.String()}
	_, rec := explain(t, s, tok, api.AuthzExplainRequest{User: "a@example.com", Groups: strs("my-org:team-a")})
	bodies = append(bodies, rec.Body.String())
	for _, body := range bodies {
		require.NotEmpty(t, body)
		for _, banned := range []string{clientSecret, bootToken, deviceID, tok, "clientSecret", "ClientSecret"} {
			assert.NotContains(t, body, banned)
		}
	}
}

func TestAuthzExplain_RequestGroupsWin(t *testing.T) {
	s, tok := authzViewServer(t)
	addSession(t, s.store, "sess-1", "sub-1", "dev@example.com", "viewer", []string{"g-old"})

	resp, rec := explain(t, s, tok, api.AuthzExplainRequest{User: "dev@example.com", Groups: strs("my-org:team-a")})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "request", resp.Identity.GroupsSource)
	assert.Equal(t, []string{"my-org:team-a"}, resp.Identity.Groups)
	assert.Equal(t, "dev@example.com", resp.Identity.User)
	assert.Equal(t, "global", resp.Scope.Kind)
	assert.True(t, resp.LoginAllowed)
	assert.Equal(t, "", resp.GlobalRole)
	assert.Empty(t, grantedBy(resp, "job.read"), "a folder grant does not hold globally")
}

func TestAuthzExplain_EmptyRequestGroupsStillWin(t *testing.T) {
	s, tok := authzViewServer(t)
	addSession(t, s.store, "sess-1", "sub-1", "dev@example.com", "viewer", []string{"my-org:team-a"})
	resp, rec := explain(t, s, tok, api.AuthzExplainRequest{User: "dev@example.com", Groups: strs()})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "request", resp.Identity.GroupsSource)
	assert.Equal(t, []string{}, resp.Identity.Groups)
	assert.False(t, resp.LoginAllowed)
}

func TestAuthzExplain_LoginGroupsUsed(t *testing.T) {
	s, tok := authzViewServer(t)
	addSession(t, s.store, "sess-old", "sub-1", "dev@example.com", "viewer", []string{"g-old"})
	addSession(t, s.store, "sess-new", "sub-1", "dev@example.com", "viewer", []string{"my-org:team-a"})

	resp, rec := explain(t, s, tok, api.AuthzExplainRequest{User: "dev@example.com", Job: "team-a/x"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "login", resp.Identity.GroupsSource)
	assert.Equal(t, []string{"my-org:team-a"}, resp.Identity.Groups)
	assert.NotEmpty(t, grantedBy(resp, "run.trigger"))

	// The sub finds the same session.
	resp, _ = explain(t, s, tok, api.AuthzExplainRequest{User: "sub-1"})
	assert.Equal(t, "login", resp.Identity.GroupsSource)
}

func TestAuthzExplain_NoSnapshot(t *testing.T) {
	s, tok := authzViewServer(t)
	// A pre-upgrade session (NULL claims) is not a snapshot.
	addSession(t, s.store, "sess-null", "sub-1", "dev@example.com", "viewer", nil)
	for _, user := range []string{"dev@example.com", "nobody@example.com"} {
		resp, rec := explain(t, s, tok, api.AuthzExplainRequest{User: user})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "none", resp.Identity.GroupsSource)
		assert.Equal(t, []string{}, resp.Identity.Groups)
		assert.Contains(t, rec.Body.String(), `"groups":[]`)
	}
}

func TestAuthzExplain_UserMatchedAsEmailAndSub(t *testing.T) {
	s, tok := authzViewServer(t)
	resp, rec := explain(t, s, tok, api.AuthzExplainRequest{User: "oncall-lead@example.com"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "admin", resp.GlobalRole)
	assert.True(t, resp.LoginAllowed)
	assert.Len(t, resp.Permissions, 25)
	assert.Equal(t, []string{`role:admin (userMap["oncall-lead@example.com"])`}, grantedBy(resp, "authz.read"))

	resp, _ = explain(t, s, tok, api.AuthzExplainRequest{User: "reader@example.com"})
	assert.Equal(t, []string{`global (users["reader@example.com"])`}, grantedBy(resp, "authz.read"))
	assert.Equal(t, "", resp.GlobalRole)
}

func TestAuthzExplain_FolderScopeEqualsJobInside(t *testing.T) {
	s, tok := authzViewServer(t)
	req := api.AuthzExplainRequest{Groups: strs("my-org:team-a", "my-org:sre")}

	req.Folder = "team-a/prod"
	byFolder, rec := explain(t, s, tok, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, api.AuthzExplainScope{Kind: "folder", Name: "team-a/prod"}, byFolder.Scope)

	req.Folder, req.Job = "", "team-a/prod/build"
	byJob, rec := explain(t, s, tok, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, api.AuthzExplainScope{Kind: "job", Name: "team-a/prod/build"}, byJob.Scope)

	assert.NotEmpty(t, byJob.Permissions)
	assert.Equal(t, byJob.Permissions, byFolder.Permissions)
	assert.Len(t, grantedBy(byFolder, "run.approve"), 2, "team-a (developer) and team-a/prod both grant it")

	// A folder given with surrounding slashes is normalised.
	req.Job, req.Folder = "", "/team-a/prod/"
	norm, _ := explain(t, s, tok, req)
	assert.Equal(t, "team-a/prod", norm.Scope.Name)

	// The sibling folder is not covered.
	req.Folder = "team-b"
	other, _ := explain(t, s, tok, req)
	assert.Empty(t, other.Permissions)
}

func TestAuthzExplain_GrantedByMatchesAudit(t *testing.T) {
	s, tok := authzViewServer(t)
	bodySeedJob(t, s.store, "team-a/x")

	resp, rec := explain(t, s, tok, api.AuthzExplainRequest{
		User: "dev@example.com", Groups: strs("my-org:team-a"), Job: "team-a/x",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	by := grantedBy(resp, "run.trigger")
	require.NotEmpty(t, by)

	rec = bodyPost(t, s, "/api/v1/runs", bearer("dev@example.com", "my-org:team-a"), api.TriggerRunRequest{JobName: "team-a/x"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	logs := auditRows(t, s.store)
	require.Len(t, logs, 1, "the explain call itself is not audited")
	assert.Equal(t, "run.trigger", logs[0].Action)
	assert.Equal(t, by[0], logs[0].GrantedBy)
}

func TestAuthzExplain_Errors(t *testing.T) {
	s, tok := authzViewServer(t)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"nothing", `{}`, "user or groups is required"},
		{"job and folder", `{"user":"a@example.com","job":"team-a/x","folder":"team-a"}`, "job and folder are mutually exclusive"},
		{"wildcard folder", `{"user":"a@example.com","folder":"team-*"}`, `invalid folder: folders are literal job-name prefixes, "*", "?", "[" are not allowed`},
		{"bad segment", `{"user":"a@example.com","folder":"team-a//x"}`, `invalid folder: segments must be non-empty and must not be "." or ".."`},
		{"bad json", `{`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := doAs(t, s, http.MethodPost, "/api/v1/authz/explain", tok, []byte(c.body))
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			if c.want != "" {
				assert.Equal(t, c.want, strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}

func TestAuthzEndpoints_AllowedCallsNotAudited(t *testing.T) {
	s, tok := authzViewServer(t)
	require.Equal(t, http.StatusOK, doAs(t, s, http.MethodGet, "/api/v1/authz/policy", tok, nil).Code)
	_, rec := explain(t, s, tok, api.AuthzExplainRequest{User: "a@example.com"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, auditRows(t, s.store))

	_, rec = explain(t, s, "tok-viewer", api.AuthzExplainRequest{User: "a@example.com"})
	require.Equal(t, http.StatusForbidden, rec.Code)
	logs := auditRows(t, s.store)
	require.Len(t, logs, 1)
	assert.Equal(t, "authz.read", logs[0].Action)
	assert.Equal(t, "authz.read", logs[0].Permission)
	assert.Equal(t, http.StatusForbidden, logs[0].Status)
	assert.Equal(t, http.MethodPost, logs[0].Method)
	assert.Equal(t, "/api/v1/authz/explain", logs[0].Path)

	require.Equal(t, http.StatusForbidden, doAs(t, s, http.MethodGet, "/api/v1/authz/policy", "tok-developer", nil).Code)
	assert.Len(t, auditRows(t, s.store), 2)
}
