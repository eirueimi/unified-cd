package dsl

import "testing"

func TestQualifyName(t *testing.T) {
	cases := []struct{ path, name, want string }{
		{"", "build", "build"},
		{"team-a", "build", "team-a/build"},
		{"team-b/edge", "test", "team-b/edge/test"},
		{"/team-a/", "build", "team-a/build"},
		{"team-a", "/build/", "team-a/build"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := QualifyName(c.path, c.name); got != c.want {
			t.Errorf("QualifyName(%q,%q)=%q want %q", c.path, c.name, got, c.want)
		}
	}
}

func TestSplitQualifiedName(t *testing.T) {
	cases := []struct{ q, wantPath, wantLeaf string }{
		{"build", "", "build"},
		{"team-a/build", "team-a", "build"},
		{"team-b/edge/test", "team-b/edge", "test"},
		{"", "", ""},
	}
	for _, c := range cases {
		p, l := SplitQualifiedName(c.q)
		if p != c.wantPath || l != c.wantLeaf {
			t.Errorf("SplitQualifiedName(%q)=(%q,%q) want (%q,%q)", c.q, p, l, c.wantPath, c.wantLeaf)
		}
	}
}

func TestMetadataQualifiedName(t *testing.T) {
	m := Metadata{Name: "build", Annotations: map[string]string{"path": "team-a"}}
	if got := m.QualifiedName(); got != "team-a/build" {
		t.Errorf("QualifiedName()=%q want team-a/build", got)
	}
	m2 := Metadata{Name: "hello"}
	if got := m2.QualifiedName(); got != "hello" {
		t.Errorf("QualifiedName()=%q want hello", got)
	}
}
