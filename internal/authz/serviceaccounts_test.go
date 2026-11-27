package authz

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestCompileServiceAccount(t *testing.T) {
	g, err := CompileServiceAccount("ci", &ServiceAccountPermissions{
		Global:  []string{"viewer"},
		Folders: []FolderGrant{{Folder: "team-a", Grants: []string{"developer"}}},
	})
	require.NoError(t, err)
	require.True(t, g.Can(JobRead, "x"))
	require.True(t, g.Can(RunTrigger, "team-a/job"))
	require.False(t, g.Can(RunTrigger, "team-b/job"))
	require.Contains(t, g.Explain(JobRead, "x")[0], "sa:ci")

	_, err = CompileServiceAccount("ci", &ServiceAccountPermissions{
		Folders: []FolderGrant{{Folder: "team-a", Grants: []string{"secret.write"}}},
	})
	require.ErrorContains(t, err, `serviceAccounts["ci"].permissions`)
	require.ErrorContains(t, err, "is global-only and cannot be granted on a folder")

	_, err = CompileServiceAccount("ci", &ServiceAccountPermissions{Global: []string{"bogus"}})
	require.ErrorContains(t, err, "unknown permission or role")

	g, err = CompileServiceAccount("ci", nil)
	require.NoError(t, err)
	require.True(t, g.IsEmpty())
}

func TestServiceAccountPermissionsUnmarshal(t *testing.T) {
	var p ServiceAccountPermissions
	require.NoError(t, yaml.Unmarshal([]byte(`
global: [viewer]
folders:
  team-a: developer
  team-a/prod: [job.read, run.trigger]
`), &p))
	require.Equal(t, []string{"viewer"}, p.Global)
	require.Equal(t, []FolderGrant{
		{Folder: "team-a", Grants: []string{"developer"}},
		{Folder: "team-a/prod", Grants: []string{"job.read", "run.trigger"}},
	}, p.Folders)

	var s ServiceAccountPermissions
	require.NoError(t, yaml.Unmarshal([]byte("global: viewer\n"), &s))
	require.Equal(t, []string{"viewer"}, s.Global)

	require.ErrorContains(t, yaml.Unmarshal([]byte("bogus: 1\n"), &s), "unknown field")
}
