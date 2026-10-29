package authz

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCatalogueHas25PermissionsInOrder(t *testing.T) {
	all := All()
	require.Len(t, all, 25)
	folder := 0
	for i, p := range all {
		if FolderScopable(p) {
			folder++
			require.Less(t, i, 9)
		}
	}
	require.Equal(t, 9, folder)
	require.Equal(t, AuthzRead, all[24])
	require.Equal(t, JobRead, all[0])
}

func TestBundles(t *testing.T) {
	v, _ := Bundle("viewer")
	d, _ := Bundle("developer")
	a, _ := Bundle("admin")
	require.ElementsMatch(t, []string{"agent.read", "appsource.read", "job.read", "vars.read", "webhook.read"}, v.Names())
	require.Equal(t, v, d.Intersect(v)) // developer is a superset of viewer
	require.Len(t, d.Names(), 15)
	require.Equal(t, AllSet(), a)
	require.False(t, d.Has(AuthzRead))
	require.False(t, d.Has(TokenManage))
	require.False(t, d.Has(SecretWrite))
	_, ok := Bundle("deny")
	require.False(t, ok)
}

func TestParseRejectsUnknown(t *testing.T) {
	_, ok := Parse("jobs.read")
	require.False(t, ok)
	p, ok := Parse("run.approve")
	require.True(t, ok)
	require.Equal(t, RunApprove, p)
}

func TestSetOperations(t *testing.T) {
	s := Set(0).With(RunTrigger, JobRead)
	require.True(t, s.Has(JobRead))
	require.False(t, s.Has(RunCancel))
	require.Equal(t, []string{"job.read", "run.trigger"}, s.Names())
	require.True(t, Set(0).IsEmpty())
	require.False(t, s.IsEmpty())
	require.Equal(t, s, s.Union(Set(0).With(JobRead)))
	require.Equal(t, Set(0).With(JobRead), s.Intersect(Set(0).With(JobRead, SecretWrite)))
	require.Equal(t, s, s.With(SecretWrite).FolderPart())
	require.Len(t, AllSet().FolderPart().Names(), 9)
	require.False(t, Set(0).Has(Permission("bogus")))
}
