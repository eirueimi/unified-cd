package dsl

import "strings"
import "testing"

func TestParseGitCredential_Valid(t *testing.T) {
	in := `apiVersion: unified-cd/v1
kind: GitCredential
metadata:
  name: gh
spec:
  host: github.com
  type: token
  secretRef: gh-token`
	gc, err := ParseGitCredential(strings.NewReader(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gc.Metadata.Name != "gh" || gc.Spec.Host != "github.com" || gc.Spec.Type != "token" || gc.Spec.SecretRef != "gh-token" {
		t.Fatalf("bad parse: %+v", gc)
	}
}

func TestParseGitCredential_Invalid(t *testing.T) {
	cases := map[string]string{
		"missing name":   "kind: GitCredential\nspec:\n  host: github.com\n  type: token\n  secretRef: s",
		"missing host":   "kind: GitCredential\nmetadata:\n  name: gh\nspec:\n  type: token\n  secretRef: s",
		"bad type":       "kind: GitCredential\nmetadata:\n  name: gh\nspec:\n  host: github.com\n  type: basic\n  secretRef: s",
		"missing secret": "kind: GitCredential\nmetadata:\n  name: gh\nspec:\n  host: github.com\n  type: token",
	}
	for name, in := range cases {
		if _, err := ParseGitCredential(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}
