package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func authzPolicyFixture() api.AuthzPolicy {
	return api.AuthzPolicy{
		OIDC:       true,
		RolesClaim: "groups",
		Bundles:    map[string][]string{"admin": {"authz.read", "job.read"}, "developer": {"job.read"}, "viewer": {"job.read"}},
		Legacy: api.AuthzLegacy{
			RoleMap:     []api.AuthzLegacyEntry{{Key: "my-org:devs", Role: "develper", Ignored: true, Permissions: []string{}}},
			UserMap:     []api.AuthzLegacyEntry{{Key: "x@example.com", Role: "superuser", Denies: true, Permissions: []string{}}},
			DefaultRole: api.AuthzLegacyEntry{Key: "defaultRole", Role: "deny", Permissions: []string{}},
		},
		Global: []api.AuthzGrantEntry{{Subject: "group", Name: "my-org:auditors", Grant: []string{"viewer", "audit.read"}, Permissions: []string{"audit.read", "job.read"}}},
		Folders: []api.AuthzFolderEntry{{Folder: "team-a", Entries: []api.AuthzGrantEntry{
			{Subject: "group", Name: "my-org:team-a", Grant: []string{"developer"}, Permissions: []string{"job.read", "run.trigger"}},
		}}},
	}
}

func runAuthz(t *testing.T, tr *captureTransport, args ...string) (string, error) {
	t.Helper()
	cfg := Config{Server: "http://test", Token: "tok"}
	cmd := newAuthzCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	cmd.SetArgs(args)
	var out, errOut strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.Execute()
	return out.String(), err
}

func policyTransport(t *testing.T) *captureTransport {
	body, err := json.Marshal(authzPolicyFixture())
	require.NoError(t, err)
	return &captureTransport{responseFor: func(string) (int, []byte) { return http.StatusOK, body }}
}

func TestAuthzShowRequest(t *testing.T) {
	tr := policyTransport(t)
	_, err := runAuthz(t, tr, "show")
	require.NoError(t, err)
	require.Len(t, tr.records, 1)
	assert.Equal(t, http.MethodGet, tr.records[0].method)
	assert.Equal(t, "/api/v1/authz/policy", tr.records[0].path)
	assert.Equal(t, "Bearer tok", tr.records[0].authorization)
}

func TestAuthzExplainRequest(t *testing.T) {
	resp, _ := json.Marshal(api.AuthzExplainResponse{})
	tr := &captureTransport{responseFor: func(string) (int, []byte) { return http.StatusOK, resp }}
	_, err := runAuthz(t, tr, "explain", "--user", "a@example.com", "--group", "g1", "--group", "g2", "--job", "team-a/x")
	require.NoError(t, err)
	require.Len(t, tr.records, 1)
	assert.Equal(t, http.MethodPost, tr.records[0].method)
	assert.Equal(t, "/api/v1/authz/explain", tr.records[0].path)
	assert.Equal(t, "Bearer tok", tr.records[0].authorization)
	assert.JSONEq(t, `{"user":"a@example.com","groups":["g1","g2"],"job":"team-a/x"}`, string(tr.records[0].body))

	tr = &captureTransport{responseFor: func(string) (int, []byte) { return http.StatusOK, resp }}
	_, err = runAuthz(t, tr, "explain", "--user", "a@example.com", "--folder", "team-a")
	require.NoError(t, err)
	var sent map[string]any
	require.NoError(t, json.Unmarshal(tr.records[0].body, &sent))
	assert.NotContains(t, sent, "groups", "without --group the body has no groups key")
	assert.Equal(t, "team-a", sent["folder"])
}

func TestAuthzOutputFormats(t *testing.T) {
	tr := policyTransport(t)

	out, err := runAuthz(t, tr, "show", "-o", "json")
	require.NoError(t, err)
	var got api.AuthzPolicy
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, authzPolicyFixture(), got)

	out, err = runAuthz(t, tr, "show", "-o", "yaml")
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(out), &doc))
	assert.Equal(t, "groups", doc["rolesClaim"])

	out, err = runAuthz(t, tr, "show")
	require.NoError(t, err)
	assert.Contains(t, out, "BUNDLES")
	assert.Contains(t, out, "LEGACY")
	assert.Contains(t, out, "GLOBAL")
	assert.Contains(t, out, "FOLDER team-a")
	assert.Contains(t, out, "SUBJECT\tNAME\tGRANT\tPERMISSIONS")
	assert.Contains(t, out, "roleMap\tmy-org:devs\tdevelper (ignored)\t-")
	assert.Contains(t, out, "group\tmy-org:team-a\tdeveloper\tjob.read,run.trigger")

	before := len(tr.records)
	_, err = runAuthz(t, tr, "show", "-o", "xml")
	require.Error(t, err)
	assert.Equal(t, `unsupported output format "xml" (use table, yaml or json)`, err.Error())
	assert.Len(t, tr.records, before, "an unsupported format makes no request")
}

func TestAuthzExplainTable(t *testing.T) {
	resp, _ := json.Marshal(api.AuthzExplainResponse{
		Identity:     api.AuthzExplainIdentity{User: "a@example.com", Groups: []string{"g1", "g2"}, GroupsSource: "request"},
		Scope:        api.AuthzExplainScope{Kind: "job", Name: "team-a/x"},
		GlobalRole:   "",
		LoginAllowed: true,
		Permissions: []api.AuthzExplainedPermission{
			{Permission: "job.read", GrantedBy: []string{"global (groups[\"g1\"])", "folder:team-a (groups[\"g2\"])"}},
		},
	})
	tr := &captureTransport{responseFor: func(string) (int, []byte) { return http.StatusOK, resp }}
	out, err := runAuthz(t, tr, "explain", "--user", "a@example.com", "--group", "g1")
	require.NoError(t, err)
	assert.Contains(t, out, "identity\ta@example.com\n")
	assert.Contains(t, out, "groups (request)\tg1,g2\n")
	assert.Contains(t, out, "scope\tjob team-a/x\n")
	assert.Contains(t, out, "global role\t-\n")
	assert.Contains(t, out, "login allowed\ttrue\n")
	assert.Contains(t, out, "PERMISSION\tGRANTED BY\n")
	assert.Contains(t, out, "job.read\tglobal (groups[\"g1\"]); folder:team-a (groups[\"g2\"])\n")
}

func TestAuthzExplainJobFolderExclusive(t *testing.T) {
	tr := policyTransport(t)
	_, err := runAuthz(t, tr, "explain", "--user", "a@example.com", "--job", "team-a/x", "--folder", "team-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "none of the others can be")
	assert.Empty(t, tr.records)
}

func TestAuthzServerErrorVerbatim(t *testing.T) {
	const body = `forbidden: missing permission "authz.read"`
	tr := &captureTransport{responseFor: func(string) (int, []byte) { return http.StatusForbidden, []byte(body) }}
	_, err := runAuthz(t, tr, "show")
	require.Error(t, err)
	assert.Equal(t, "server error: "+body, err.Error())

	_, err = runAuthz(t, tr, "explain", "--user", "a@example.com")
	require.Error(t, err)
	assert.Equal(t, "server error: "+body, err.Error())
}
