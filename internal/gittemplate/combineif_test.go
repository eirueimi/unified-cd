package gittemplate

import "testing"

func TestCombineIf(t *testing.T) {
	cases := []struct{ outer, inner, want string }{
		{"", "", ""},
		{"failure()", "", "failure()"},
		{"", "params.x == \"1\"", "params.x == \"1\""},
		{"failure()", "params.x == \"1\"", "(failure()) && (params.x == \"1\")"},
	}
	for _, c := range cases {
		if got := combineIf(c.outer, c.inner); got != c.want {
			t.Errorf("combineIf(%q,%q) = %q, want %q", c.outer, c.inner, got, c.want)
		}
	}
}
