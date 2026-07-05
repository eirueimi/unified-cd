package controller

import "testing"

func TestRelDir(t *testing.T) {
	cases := []struct{ specPath, filePath, want string }{
		{"jobs/", "jobs/build.yaml", ""},
		{"jobs/", "jobs/team-a/build.yaml", "team-a"},
		{"jobs/", "jobs/team-b/edge/test.yaml", "team-b/edge"},
		{"jobs", "jobs/team-a/build.yaml", "team-a"},
		{"", "team-a/build.yaml", "team-a"},
	}
	for _, c := range cases {
		if got := relDir(c.specPath, c.filePath); got != c.want {
			t.Errorf("relDir(%q,%q)=%q want %q", c.specPath, c.filePath, got, c.want)
		}
	}
}
