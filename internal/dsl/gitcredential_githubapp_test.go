package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const gitHubAppCredYAML = `apiVersion: unified-cd/v1
kind: GitCredential
metadata:
  name: gh-app-release
spec:
  host: github.com
  type: githubApp
  secretRef: github-app-key
  controllerUse: true
  githubApp:
    appId: 123456
    installationId: 7890123
    apiURL: https://api.github.com
  jobAccess:
    folders: [team-a, platform/tools]
    repositories: [acme/app, acme/infra-*]
    permissions:
      contents: write
      pull_requests: write
`

func TestGitCredentialValidate_GitHubApp(t *testing.T) {
	gc, err := ParseGitCredential(strings.NewReader(gitHubAppCredYAML))
	require.NoError(t, err)
	assert.Equal(t, int64(123456), gc.Spec.GitHubApp.AppID)
	assert.Equal(t, "write", gc.Spec.JobAccess.Permissions["contents"])

	cases := []struct {
		name    string
		mutate  func(gc *GitCredential)
		wantErr string
	}{
		{"type enum", func(gc *GitCredential) { gc.Spec.Type = "bogus" }, `spec.type must be "token", "sshKey" or "githubApp", got "bogus"`},
		{"githubApp required", func(gc *GitCredential) { gc.Spec.GitHubApp = nil }, `spec.githubApp is required for type "githubApp"`},
		{"githubApp only for githubApp", func(gc *GitCredential) { gc.Spec.Type = "token"; gc.Spec.JobAccess = nil }, `spec.githubApp is only valid for type "githubApp"`},
		{"appId positive", func(gc *GitCredential) { gc.Spec.GitHubApp.AppID = 0 }, `spec.githubApp.appId must be a positive integer`},
		{"installationId non-negative", func(gc *GitCredential) { gc.Spec.GitHubApp.InstallationID = -1 }, `spec.githubApp.installationId must not be negative`},
		{"apiURL https", func(gc *GitCredential) { gc.Spec.GitHubApp.APIURL = "http://ghe.example.com/api/v3" }, `spec.githubApp.apiURL must be an https URL without credentials, query or fragment`},
		{"apiURL userinfo", func(gc *GitCredential) { gc.Spec.GitHubApp.APIURL = "https://u:p@ghe.example.com/api/v3" }, `spec.githubApp.apiURL must be an https URL without credentials, query or fragment`},
		{"apiURL query", func(gc *GitCredential) { gc.Spec.GitHubApp.APIURL = "https://ghe.example.com/api/v3?x=1" }, `spec.githubApp.apiURL must be an https URL without credentials, query or fragment`},
		{"knownHosts not with githubApp", func(gc *GitCredential) { gc.Spec.KnownHosts = "github.com ssh-ed25519 AAAA" }, `spec.knownHosts is only valid for type "sshKey"`},
		{"jobAccess only githubApp", func(gc *GitCredential) { gc.Spec.Type = "token"; gc.Spec.GitHubApp = nil }, `spec.jobAccess is only valid for type "githubApp"`},
		{"folders required", func(gc *GitCredential) { gc.Spec.JobAccess.Folders = []string{} }, `spec.jobAccess.folders must not be empty`},
		{"repositories required", func(gc *GitCredential) { gc.Spec.JobAccess.Repositories = []string{} }, `spec.jobAccess.repositories must not be empty`},
		{"permissions required", func(gc *GitCredential) { gc.Spec.JobAccess.Permissions = map[string]string{} }, `spec.jobAccess.permissions must not be empty`},
		{"single owner", func(gc *GitCredential) { gc.Spec.JobAccess.Repositories = []string{"acme/app", "widget/lib"} }, `spec.jobAccess.repositories must name a single owner, got "acme" and "widget"`},
		{"pattern shape", func(gc *GitCredential) { gc.Spec.JobAccess.Repositories = []string{"acme/*app"} }, `spec.jobAccess.repositories[0] "acme/*app": want "owner/name", "owner/*" or "owner/prefix*"`},
		{"level", func(gc *GitCredential) { gc.Spec.JobAccess.Permissions = map[string]string{"contents": "maintain"} }, `spec.jobAccess.permissions.contents must be "read", "write" or "admin", got "maintain"`},
		{"permission name", func(gc *GitCredential) { gc.Spec.JobAccess.Permissions = map[string]string{"Contents!": "read"} }, `spec.jobAccess.permissions: invalid permission name "Contents!"`},
		{"name vs secret pattern", func(gc *GitCredential) { gc.Metadata.Name = "1app" }, `metadata.name "1app" must match the secret-name pattern ^[A-Za-z_][A-Za-z0-9_-]*$ when spec.jobAccess is set`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, err := ParseGitCredential(strings.NewReader(gitHubAppCredYAML))
			require.NoError(t, err)
			c.mutate(g)
			err = g.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErr)
		})
	}
}

func TestGitCredentialControllerUseDefault(t *testing.T) {
	base := "apiVersion: unified-cd/v1\nkind: GitCredential\nmetadata:\n  name: gh\nspec:\n  host: github.com\n  type: token\n  secretRef: s\n"
	gc, err := ParseGitCredential(strings.NewReader(base))
	require.NoError(t, err)
	assert.True(t, gc.Spec.ControllerUseOrDefault())
	gc, err = ParseGitCredential(strings.NewReader(base + "  controllerUse: false\n"))
	require.NoError(t, err)
	assert.False(t, gc.Spec.ControllerUseOrDefault())
	ssh := "apiVersion: unified-cd/v1\nkind: GitCredential\nmetadata:\n  name: gh\nspec:\n  host: github.com\n  type: sshKey\n  secretRef: s\n  controllerUse: false\n"
	gc, err = ParseGitCredential(strings.NewReader(ssh))
	require.NoError(t, err)
	assert.False(t, gc.Spec.ControllerUseOrDefault())
}

func TestParseRepositoryPatterns(t *testing.T) {
	owner, names, err := ParseRepositoryPatterns([]string{"acme/app", "acme/infra-*"})
	require.NoError(t, err)
	assert.Equal(t, "acme", owner)
	assert.Equal(t, []string{"app", "infra-*"}, names)

	owner, names, err = ParseRepositoryPatterns([]string{"acme/*"})
	require.NoError(t, err)
	assert.Equal(t, "acme", owner)
	assert.Equal(t, []string{"*"}, names)

	owner, _, err = ParseRepositoryPatterns([]string{"Acme/x", "acme/y"})
	require.NoError(t, err)
	assert.Equal(t, "Acme", owner)

	_, _, err = ParseRepositoryPatterns(nil)
	require.Error(t, err)
}

func TestMatchRepositoryName(t *testing.T) {
	assert.True(t, MatchRepositoryName("*", "anything"))
	assert.True(t, MatchRepositoryName("infra-*", "Infra-DB"))
	assert.False(t, MatchRepositoryName("infra-*", "infra"))
	assert.True(t, MatchRepositoryName("App", "app"))
	assert.False(t, MatchRepositoryName("app", "app2"))
	assert.True(t, RepositoryAllowed([]string{"acme/app"}, "ACME", "App"))
	assert.False(t, RepositoryAllowed([]string{"acme/app"}, "widget", "app"))
	assert.True(t, RepositoryAllowed([]string{"acme/app", "acme/infra-*"}, "acme", "infra-db"))
	assert.False(t, RepositoryAllowed([]string{"acme/app"}, "acme", "other"))
}
