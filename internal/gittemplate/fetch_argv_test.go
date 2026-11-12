package gittemplate

import (
	"context"
	"strings"
	"testing"
)

// TestResolveCommitSHA_RejectsDashURL and TestFetchDir_RejectsDashURL exercise
// the argv-defense-in-depth guards: a repoURL beginning with '-' must be
// rejected BEFORE any git subprocess is exec'd, since git would otherwise
// interpret it as a command-line option (git option injection).
func TestResolveCommitSHA_RejectsDashURL(t *testing.T) {
	f := &Fetcher{}
	_, err := f.ResolveCommitSHA(context.Background(), "--upload-pack=touch /tmp/pwn", "main", Credential{})
	if err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Fatalf("dash repoURL must be rejected before exec, got %v", err)
	}
}

func TestFetchDir_RejectsDashURL(t *testing.T) {
	f := &Fetcher{}
	_, err := f.FetchDir(context.Background(), "--upload-pack=x", "main", "p", Credential{})
	if err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Fatalf("dash repoURL must be rejected before exec, got %v", err)
	}
}

func TestBuildHTTPSURL_Username(t *testing.T) {
	uri := URI{Host: "github.com", Owner: "acme", Repo: "app", Ref: "v1", Path: "x.yaml"}
	cases := []struct {
		cred Credential
		want string
	}{
		{Credential{Token: "t", Username: GitHubAppUser}, "https://x-access-token@github.com/acme/app.git"},
		{Credential{Token: "t"}, "https://oauth2@github.com/acme/app.git"},
		{Credential{}, "https://github.com/acme/app.git"},
	}
	for _, c := range cases {
		if got := buildHTTPSURL(uri, c.cred); got != c.want {
			t.Errorf("buildHTTPSURL(%+v) = %q, want %q", c.cred.Username, got, c.want)
		}
	}
}

func TestInsertUser_Username(t *testing.T) {
	if got := insertUser("https://github.com/acme/app.git", Credential{Token: "t", Username: GitHubAppUser}); got != "https://x-access-token@github.com/acme/app.git" {
		t.Errorf("got %q", got)
	}
	if got := insertUser("https://github.com/acme/app.git", Credential{Token: "t"}); got != "https://oauth2@github.com/acme/app.git" {
		t.Errorf("got %q", got)
	}
}
