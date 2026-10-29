package authz

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type policyDoc struct {
	Permissions *PolicyConfig `yaml:"permissions"`
}

func decodePolicy(t *testing.T, src string) (*PolicyConfig, error) {
	t.Helper()
	var doc policyDoc
	dec := yaml.NewDecoder(strings.NewReader(src))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return doc.Permissions, nil
}

func compileYAML(t *testing.T, src, issuer string) (*Policy, error) {
	t.Helper()
	cfg, err := decodePolicy(t, src)
	require.NoError(t, err)
	p, _, err := Compile(cfg, Legacy{}, issuer)
	return p, err
}

func TestCompileStartupErrors(t *testing.T) {
	const idp = "https://idp.example.com"
	tests := []struct {
		name, yaml, issuer, want string
	}{
		{"unknown permission", "permissions:\n  folders:\n    team-a:\n      groups:\n        \"my-org:team-a\": jobs.read\n", idp,
			`oidc.permissions.folders["team-a"].groups["my-org:team-a"]: unknown permission or role "jobs.read"`},
		{"global-only on folder", "permissions:\n  folders:\n    team-a:\n      groups:\n        \"my-org:team-a\": [secret.write]\n", idp,
			`oidc.permissions.folders["team-a"].groups["my-org:team-a"]: permission "secret.write" is global-only and cannot be granted on a folder`},
		{"bad segment", "permissions:\n  folders:\n    team-a//x:\n      groups: {g: viewer}\n", idp,
			`oidc.permissions.folders["team-a//x"]: invalid folder: segments must be non-empty and must not be "." or ".."`},
		{"wildcard", "permissions:\n  folders:\n    team-*:\n      groups: {g: viewer}\n", idp,
			`oidc.permissions.folders["team-*"]: invalid folder: folders are literal job-name prefixes, "*", "?", "[" are not allowed`},
		{"whitespace", "permissions:\n  folders:\n    team a:\n      groups: {g: viewer}\n", idp,
			`oidc.permissions.folders["team a"]: invalid folder: whitespace is not allowed`},
		{"duplicate folder", "permissions:\n  folders:\n    team-a:\n      groups: {g: viewer}\n    /team-a/:\n      groups: {g: viewer}\n", idp,
			`oidc.permissions.folders: "team-a" and "/team-a/" are the same folder`},
		{"empty entry", "permissions:\n  folders:\n    team-a: {}\n", idp,
			`oidc.permissions.folders["team-a"]: no groups or users`},
		{"empty grant", "permissions:\n  global:\n    groups:\n      \"my-org:x\": []\n", idp,
			`oidc.permissions.global.groups["my-org:x"]: empty grant`},
		{"no issuer", "permissions:\n  global:\n    groups: {g: viewer}\n", "",
			`oidc.permissions is set but oidc.issuer is not`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compileYAML(t, tc.yaml, tc.issuer)
			require.EqualError(t, err, tc.want)
		})
	}

	t.Run("unknown key", func(t *testing.T) {
		_, err := decodePolicy(t, "permissions:\n  deny: {}\n")
		require.EqualError(t, err, `oidc.permissions: unknown field "deny"`)
	})
}

func TestDecodeErrors(t *testing.T) {
	tests := []struct{ name, yaml, want string }{
		{"unknown in global", "permissions:\n  global:\n    deny: x\n", `oidc.permissions.global: unknown field "deny"`},
		{"unknown in folder", "permissions:\n  folders:\n    team-a:\n      roles: {}\n", `oidc.permissions.folders["team-a"]: unknown field "roles"`},
		{"duplicate group key", "permissions:\n  global:\n    groups:\n      g: viewer\n      g: admin\n", `oidc.permissions.global.groups: duplicate key "g"`},
		{"duplicate folder key", "permissions:\n  folders:\n    a: {groups: {g: viewer}}\n    a: {groups: {g: viewer}}\n", `oidc.permissions.folders: duplicate key "a"`},
		{"mapping as grant", "permissions:\n  global:\n    groups:\n      g: {a: b}\n", `oidc.permissions.global.groups["g"]: grant must be a role or permission name or a list of them`},
		{"nested list", "permissions:\n  global:\n    groups:\n      g: [[viewer]]\n", `oidc.permissions.global.groups["g"]: grant must be a role or permission name or a list of them`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodePolicy(t, tc.yaml)
			require.EqualError(t, err, tc.want)
		})
	}
}

func TestDecodeKeepsOrderAndAcceptsAnchors(t *testing.T) {
	cfg, err := decodePolicy(t, `
permissions:
  folders:
    zeta:
      groups: {b: &dev [job.read, run.trigger], a: developer}
    alpha:
      users: {u@example.com: *dev}
`)
	require.NoError(t, err)
	require.Equal(t, "zeta", cfg.Folders[0].Key)
	require.Equal(t, "alpha", cfg.Folders[1].Key)
	require.Equal(t, "b", cfg.Folders[0].Groups[0].Key)
	require.Equal(t, []string{"job.read", "run.trigger"}, cfg.Folders[1].Users[0].Grants)
}

func TestCompileBundleOnFolderIsFolderPartOnly(t *testing.T) {
	pol, err := compileYAML(t, "permissions:\n  folders:\n    team-a:\n      groups:\n        g: admin\n", "https://idp.example.com")
	require.NoError(t, err)
	g, ok := pol.Match(Identity{Values: []string{"g"}})
	require.True(t, ok)
	require.Equal(t, GrantsView{Global: []string{}, Folders: map[string][]string{"team-a": AllSet().FolderPart().Names()}}, g.View())
}

func TestCompileExplicitAuthzReadOnFolderIsError(t *testing.T) {
	_, err := compileYAML(t, "permissions:\n  folders:\n    team-a:\n      groups:\n        g: [authz.read]\n", "https://idp.example.com")
	require.EqualError(t, err, `oidc.permissions.folders["team-a"].groups["g"]: permission "authz.read" is global-only and cannot be granted on a folder`)
}

func TestMatchUsersMatchesEmailAndSubAdditively(t *testing.T) {
	pol, err := compileYAML(t, "permissions:\n  global:\n    users:\n      a@example.com: [job.read]\n      sub-1: [run.trigger]\n", "https://idp.example.com")
	require.NoError(t, err)
	g, ok := pol.Match(Identity{Email: "a@example.com", Sub: "sub-1"})
	require.True(t, ok)
	require.True(t, g.Can(JobRead, "x"))
	require.True(t, g.Can(RunTrigger, "x"))
	require.False(t, pol.HasGroupKeyedGrants())
}

func TestMatchUnmatchedIsFalse(t *testing.T) {
	pol, err := compileYAML(t, "permissions:\n  global:\n    groups: {g: viewer}\n", "https://idp.example.com")
	require.NoError(t, err)
	g, ok := pol.Match(Identity{Email: "x@example.com"})
	require.False(t, ok)
	require.True(t, g.IsEmpty())
	require.True(t, pol.HasGroupKeyedGrants())
	require.False(t, pol.HasFolderGrants())
}

func TestMatchSources(t *testing.T) {
	pol, err := compileYAML(t, "permissions:\n  global:\n    groups: {g: viewer}\n  folders:\n    /team-a/:\n      users: {u@example.com: job.read}\n", "https://idp.example.com")
	require.NoError(t, err)
	require.True(t, pol.HasFolderGrants())
	g, _ := pol.Match(Identity{Values: []string{"g"}, Email: "u@example.com"})
	require.Equal(t, []string{`global (groups["g"])`, `folder:team-a (users["u@example.com"])`}, g.Explain(JobRead, "team-a/build"))
}

func TestMatchNilPolicy(t *testing.T) {
	var p *Policy
	g, ok := p.Match(Identity{Email: "x"})
	require.False(t, ok)
	require.True(t, g.IsEmpty())
	require.False(t, p.Configured())
}

func TestCompileLegacyUnknownRole(t *testing.T) {
	legacy := Legacy{RoleMap: map[string]string{"my-org:devs": "develper", "ok": "viewer", "d": "deny"}}
	want := `oidc.roleMap["my-org:devs"]: unknown role "develper" is ignored (valid: admin, developer, viewer)`

	_, warnings, err := Compile(nil, legacy, "")
	require.NoError(t, err)
	require.Equal(t, []string{want}, warnings)

	cfg, err := decodePolicy(t, "permissions:\n  global:\n    groups: {g: viewer}\n")
	require.NoError(t, err)
	_, _, err = Compile(cfg, legacy, "https://idp.example.com")
	require.EqualError(t, err, want)
}

func TestCompileNilConfigIsEmptyPolicy(t *testing.T) {
	p, w, err := Compile(nil, Legacy{}, "")
	require.NoError(t, err)
	require.Empty(t, w)
	require.False(t, p.Configured())
}
