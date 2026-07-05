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
	cases := []struct {
		name    string
		in      string
		wantErr string // substring identifying which validation fired
	}{
		{"missing name", "apiVersion: unified-cd/v1\nkind: GitCredential\nspec:\n  host: github.com\n  type: token\n  secretRef: s", "metadata.name is required"},
		{"missing host", "apiVersion: unified-cd/v1\nkind: GitCredential\nmetadata:\n  name: gh\nspec:\n  type: token\n  secretRef: s", "spec.host"},
		{"bad type", "apiVersion: unified-cd/v1\nkind: GitCredential\nmetadata:\n  name: gh\nspec:\n  host: github.com\n  type: basic\n  secretRef: s", "spec.type"},
		{"missing secret", "apiVersion: unified-cd/v1\nkind: GitCredential\nmetadata:\n  name: gh\nspec:\n  host: github.com\n  type: token", "spec.secretRef"},
		{"wrong apiVersion", "apiVersion: bad\nkind: GitCredential\nmetadata:\n  name: gh\nspec:\n  host: github.com\n  type: token\n  secretRef: s", "apiVersion"},
		{"wrong kind", "apiVersion: unified-cd/v1\nkind: Schedule\nmetadata:\n  name: gh\nspec:\n  host: github.com\n  type: token\n  secretRef: s", "kind"},
		{"unknown field", "apiVersion: unified-cd/v1\nkind: GitCredential\nmetadata:\n  name: gh\nspec:\n  host: github.com\n  type: token\n  sceretRef: s", "sceretRef"},
		{"invalid name", "apiVersion: unified-cd/v1\nkind: GitCredential\nmetadata:\n  name: \"Foo Bar!\"\nspec:\n  host: github.com\n  type: token\n  secretRef: s", "is invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseGitCredential(strings.NewReader(c.in))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantErr)
			}
		})
	}
}
