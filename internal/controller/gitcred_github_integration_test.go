package controller

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/gittemplate"
	"github.com/eirueimi/unified-cd/internal/gittoken"
)

// TestGitHubAppRealInstallation confirms against a real GitHub App
// installation that a controller token fetched through gittoken works with
// git over HTTPS using the x-access-token user. It runs only when
//
//	UNIFIED_TEST_GITHUB_APP_ID        the App ID
//	UNIFIED_TEST_GITHUB_APP_KEY_FILE  path to the App's PEM private key
//	UNIFIED_TEST_GITHUB_APP_REPO      owner/repo of a private repository the App is installed on
//
// are all set.
func TestGitHubAppRealInstallation(t *testing.T) {
	appIDStr := os.Getenv("UNIFIED_TEST_GITHUB_APP_ID")
	keyFile := os.Getenv("UNIFIED_TEST_GITHUB_APP_KEY_FILE")
	repoSlug := os.Getenv("UNIFIED_TEST_GITHUB_APP_REPO")
	if appIDStr == "" || keyFile == "" || repoSlug == "" {
		t.Skip("set UNIFIED_TEST_GITHUB_APP_ID, UNIFIED_TEST_GITHUB_APP_KEY_FILE and UNIFIED_TEST_GITHUB_APP_REPO to run against a real GitHub App installation")
	}
	appID, err := strconv.ParseInt(appIDStr, 10, 64)
	if err != nil {
		t.Fatalf("UNIFIED_TEST_GITHUB_APP_ID: %v", err)
	}
	owner, repo, ok := strings.Cut(repoSlug, "/")
	if !ok || owner == "" || repo == "" {
		t.Fatalf("UNIFIED_TEST_GITHUB_APP_REPO must be owner/repo, got %q", repoSlug)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	src := gittoken.NewIssuer(nil, nil).Source(gittoken.GitHubApp{
		Name:  "integration",
		AppID: appID,
		PrivateKey: func(context.Context) ([]byte, error) {
			return os.ReadFile(keyFile)
		},
	}, gittoken.ControllerMinRemaining)
	tok, err := src.Token(ctx, gittoken.TokenRequest{
		Owner: owner, Repositories: []string{repo}, Permissions: map[string]string{"contents": "read"},
	})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	sha, err := gittemplate.NewFetcher().ResolveCommitSHA(ctx, "https://github.com/"+repoSlug+".git", "HEAD",
		gittemplate.Credential{Token: tok.Value, Username: gittemplate.GitHubAppUser})
	if err != nil {
		t.Fatalf("ls-remote with installation token: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(sha) {
		t.Fatalf("want a 40-hex SHA, got %q", sha)
	}
}
