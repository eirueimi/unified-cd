package authz

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanFixture(t *testing.T) {
	b, err := os.ReadFile("../../web/src/lib/authz-cases.json")
	require.NoError(t, err)
	var cases []struct {
		Name       string
		Grants     GrantsView
		Permission string
		Job        string
		Want       bool
	}
	require.NoError(t, json.Unmarshal(b, &cases))
	require.NotEmpty(t, cases)
	for _, c := range cases {
		g, err := FromView(c.Grants)
		require.NoError(t, err, c.Name)
		require.Equal(t, c.Want, g.Can(Permission(c.Permission), c.Job), c.Name)
	}
}

const (
	byRole   = `role:viewer (roleMap["my-org:all"])`
	byFolder = `folder:team-a (groups["my-org:team-a"])`
)

func sampleGrants() Grants {
	var g Grants
	g.Add("", Set(0).With(JobRead), byRole)
	g.Add("team-a", Set(0).With(JobRead, RunTrigger), byFolder)
	return g
}

func TestDecideReportsFirstSourceAndExplainAll(t *testing.T) {
	g := sampleGrants()
	ok, by := g.Decide(JobRead, "team-a/x")
	require.True(t, ok)
	require.Equal(t, byRole, by)
	require.Len(t, g.Explain(JobRead, "team-a/x"), 2)
	ok, _ = g.Decide(RunTrigger, "team-b/x")
	require.False(t, ok)
	require.True(t, g.HeldAnywhere(RunTrigger))
	require.False(t, g.Can(RunTrigger, ""))
	require.False(t, g.HeldAnywhere(SecretWrite))
}

func TestDecideOrdersGlobalThenShallowFolderThenInsertion(t *testing.T) {
	var g Grants
	g.Add("team-a/sub", Set(0).With(JobRead), "inner")
	g.Add("team-a", Set(0).With(JobRead), "outer-1")
	g.Add("", Set(0).With(JobRead), "global")
	g.Add("team-a", Set(0).With(JobRead), "outer-2")
	ok, by := g.Decide(JobRead, "team-a/sub/x")
	require.True(t, ok)
	require.Equal(t, "global", by)
	require.Equal(t, []string{"global", "outer-1", "outer-2", "inner"}, g.Explain(JobRead, "team-a/sub/x"))

	var h Grants
	h.Add("team-a/sub", Set(0).With(JobRead), "inner")
	h.Add("team-a", Set(0).With(JobRead), "outer")
	_, by = h.Decide(JobRead, "team-a/sub/x")
	require.Equal(t, "outer", by)
}

func TestFolderGrantAddedBeforeGlobalReportsGlobalFirst(t *testing.T) {
	var g Grants
	g.Add("team-a", Set(0).With(JobRead), "folder-source")
	g.Add("", Set(0).With(JobRead), "global-source")
	ok, by := g.Decide(JobRead, "team-a/x")
	require.True(t, ok)
	require.Equal(t, "global-source", by)
	require.Equal(t, []string{"global-source", "folder-source"}, g.Explain(JobRead, "team-a/x"))
}

func TestUnion(t *testing.T) {
	a := sampleGrants()
	var b Grants
	b.Add("team-b", Set(0).With(RunCancel), "b")
	u := a.Union(b)
	require.True(t, u.Can(RunCancel, "team-b/x"))
	require.True(t, u.Can(RunTrigger, "team-a/x"))
	require.False(t, a.Can(RunCancel, "team-b/x"), "receiver must not be mutated")
	require.False(t, b.Can(RunTrigger, "team-a/x"))
	require.Len(t, u.Explain(JobRead, "team-a/x"), 2)
}

func TestRestrictIntersectsEveryScopeAndSuffixes(t *testing.T) {
	g := sampleGrants()
	r := g.Restrict(Set(0).With(JobRead), " & pat-ceiling")
	require.True(t, r.Can(JobRead, "team-a/x"))
	require.False(t, r.Can(RunTrigger, "team-a/x"))
	require.False(t, r.HeldAnywhere(RunTrigger))
	ok, by := r.Decide(JobRead, "team-a/x")
	require.True(t, ok)
	require.Equal(t, byRole+" & pat-ceiling", by)
	require.Equal(t, []string{byRole + " & pat-ceiling", byFolder + " & pat-ceiling"}, r.Explain(JobRead, "team-a/x"))
	// the original is untouched
	require.True(t, g.Can(RunTrigger, "team-a/x"))
	// an empty suffix leaves grantedBy unchanged
	_, by = g.Restrict(AllSet(), "").Decide(JobRead, "x")
	require.Equal(t, byRole, by)
	// a ceiling that removes everything yields empty grants
	require.True(t, g.Restrict(Set(0), "").IsEmpty())
}

func TestViewOmitsEmptyFoldersAndSortsNames(t *testing.T) {
	g := sampleGrants()
	g.Add("team-empty", Set(0), "nothing")
	v := g.View()
	require.Equal(t, []string{"job.read"}, v.Global)
	require.Equal(t, map[string][]string{"team-a": {"job.read", "run.trigger"}}, v.Folders)

	var e Grants
	ev := e.View()
	require.NotNil(t, ev.Global)
	require.NotNil(t, ev.Folders)
	require.Equal(t, GrantsView{Global: []string{}, Folders: map[string][]string{}}, ev)
	raw, err := json.Marshal(ev)
	require.NoError(t, err)
	require.JSONEq(t, `{"global":[],"folders":{}}`, string(raw))
}

func TestFromViewRoundTripAndErrors(t *testing.T) {
	g := sampleGrants()
	back, err := FromView(g.View())
	require.NoError(t, err)
	require.Equal(t, g.View(), back.View())
	_, by := back.Decide(JobRead, "x")
	require.Equal(t, "view", by)

	_, err = FromView(GrantsView{Global: []string{"jobs.read"}})
	require.Error(t, err)
	_, err = FromView(GrantsView{Folders: map[string][]string{"team-a": {"secret.write"}}})
	require.Error(t, err)
	_, err = FromView(GrantsView{Folders: map[string][]string{"team-*": {"job.read"}}})
	require.Error(t, err)
}

func TestRoleAndAllGrants(t *testing.T) {
	require.True(t, RoleGrants("", "x").IsEmpty())
	require.True(t, RoleGrants("deny", "x").IsEmpty())
	g := RoleGrants("viewer", "role:viewer")
	require.True(t, g.Can(JobRead, "x"))
	require.False(t, g.Can(RunTrigger, "x"))
	require.True(t, AllGrants("all").Can(AuthzRead, ""))
}
