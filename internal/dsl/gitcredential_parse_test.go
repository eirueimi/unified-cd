package dsl

import "fmt"
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

func TestParseGitCredential_KnownHosts(t *testing.T) {
	base := "apiVersion: unified-cd/v1\nkind: GitCredential\nmetadata:\n  name: gl\nspec:\n  host: gitlab.example.com\n  type: %s\n  secretRef: s\n  knownHosts: |\n    %s\n"
	line := "gitlab.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample"

	gc, err := ParseGitCredential(strings.NewReader(fmt.Sprintf(base, "sshKey", line)))
	if err != nil {
		t.Fatalf("valid knownHosts rejected: %v", err)
	}
	if !strings.Contains(gc.Spec.KnownHosts, line) {
		t.Fatalf("knownHosts not parsed: %q", gc.Spec.KnownHosts)
	}

	if _, err := ParseGitCredential(strings.NewReader(fmt.Sprintf(base, "token", line))); err == nil || !strings.Contains(err.Error(), "only valid for type") {
		t.Errorf("knownHosts on a token credential must be rejected, got %v", err)
	}
	if _, err := ParseGitCredential(strings.NewReader(fmt.Sprintf(base, "sshKey", "gitlab.example.com"))); err == nil || !strings.Contains(err.Error(), "knownHosts line 1") {
		t.Errorf("malformed knownHosts line must be rejected, got %v", err)
	}
}

func TestValidateKnownHosts_SizeCap(t *testing.T) {
	line := "gitlab.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample\n"
	if err := ValidateKnownHosts(strings.Repeat(line, 2)); err != nil {
		t.Fatalf("small block rejected: %v", err)
	}
	if err := ValidateKnownHosts(strings.Repeat(line, MaxKnownHostsBytes/len(line)+2)); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized knownHosts must be rejected, got %v", err)
	}
}
