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
	_, err := f.ResolveCommitSHA(context.Background(), "--upload-pack=touch /tmp/pwn", "main", "", "")
	if err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Fatalf("dash repoURL must be rejected before exec, got %v", err)
	}
}

func TestFetchDir_RejectsDashURL(t *testing.T) {
	f := &Fetcher{}
	_, err := f.FetchDir(context.Background(), "--upload-pack=x", "main", "p", "", "")
	if err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Fatalf("dash repoURL must be rejected before exec, got %v", err)
	}
}
