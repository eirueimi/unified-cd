package imageref

import "testing"

func TestValidate(t *testing.T) {
	ok := []string{
		"alpine", "alpine:3.20", "ghcr.io/org/img:v1", "localhost:5000/team/app:tag",
		"registry.example.com:443/a/b/c@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"img:1.0@sha256:abc",
	}
	for _, s := range ok {
		if err := Validate(s); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", s, err)
		}
	}
	bad := []string{"", "-v", "--privileged", "-", "a b", " alpine", "alpine\n", "al\x00pine", "a\tb", "alpine x"}
	for _, s := range bad {
		if err := Validate(s); err == nil {
			t.Errorf("Validate(%q) = nil, want error", s)
		}
	}
}
