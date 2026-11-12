package store

import (
	"context"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_GitCredentialKnownHosts(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	const kh = "gitlab.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample"

	require.NoError(t, pg.UpsertGitCredential(ctx, GitCredential{Name: "gl", Host: "gitlab.example.com", CredType: "sshKey", SecretRef: "gl-key", KnownHosts: kh, ControllerUse: true}))
	gc, err := pg.GetGitCredentialByHost(ctx, "gitlab.example.com")
	require.NoError(t, err)
	require.NotNil(t, gc)
	assert.Equal(t, kh, gc.KnownHosts)

	// Re-applying without knownHosts clears the pin (declarative semantics).
	require.NoError(t, pg.UpsertGitCredential(ctx, GitCredential{Name: "gl", Host: "gitlab.example.com", CredType: "sshKey", SecretRef: "gl-key", ControllerUse: true}))
	list, err := pg.ListGitCredentials(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "", list[0].KnownHosts)
}

func TestPostgres_GitCredentialGitHubAppRoundTrip(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	in := GitCredential{Name: "gh-app", Host: "github.com", CredType: "githubApp", SecretRef: "github-app-key", ControllerUse: true,
		GitHubApp: &api.GitHubAppConfig{AppID: 123456, InstallationID: 7890123, APIURL: "https://api.github.com"},
		JobAccess: &api.GitJobAccess{Folders: []string{"team-a"}, Repositories: []string{"acme/app", "acme/infra-*"},
			Permissions: map[string]string{"contents": "write"}}}
	require.NoError(t, pg.UpsertGitCredential(ctx, in))
	got, err := pg.GetGitCredential(ctx, "gh-app")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, in.GitHubApp, got.GitHubApp)
	assert.Equal(t, in.JobAccess, got.JobAccess)
	assert.True(t, got.ControllerUse)
	list, err := pg.ListGitCredentials(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, in.JobAccess, list[0].JobAccess)
	assert.Equal(t, in.GitHubApp, list[0].GitHubApp)
}

func TestPostgres_GetGitCredentialMissingIsNil(t *testing.T) {
	pg := NewTestPostgres(t)
	got, err := pg.GetGitCredential(context.Background(), "nope")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestPostgres_GetGitCredentialByHostSkipsControllerUseFalse(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertGitCredential(ctx, GitCredential{Name: "older", Host: "github.com", CredType: "token", SecretRef: "a", ControllerUse: true}))
	require.NoError(t, pg.UpsertGitCredential(ctx, GitCredential{Name: "newer", Host: "github.com", CredType: "token", SecretRef: "b", ControllerUse: false}))
	got, err := pg.GetGitCredentialByHost(ctx, "github.com")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "older", got.Name)

	require.NoError(t, pg.UpsertGitCredential(ctx, GitCredential{Name: "older", Host: "github.com", CredType: "token", SecretRef: "a", ControllerUse: false}))
	got, err = pg.GetGitCredentialByHost(ctx, "github.com")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestPostgres_GitCredentialTokenTypeDefaults(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertGitCredential(ctx, GitCredential{Name: "tok", Host: "github.com", CredType: "token", SecretRef: "a", ControllerUse: false}))
	got, err := pg.GetGitCredential(ctx, "tok")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Nil(t, got.GitHubApp)
	assert.Nil(t, got.JobAccess)
	assert.False(t, got.ControllerUse)
}

func TestPostgres_GitCredentialCheckAcceptsGitHubAppRejectsOther(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertGitCredential(ctx, GitCredential{Name: "ok", Host: "github.com", CredType: "githubApp", SecretRef: "a"}))
	assert.Error(t, pg.UpsertGitCredential(ctx, GitCredential{Name: "bad", Host: "github.com", CredType: "bogus", SecretRef: "a"}))
}

func TestPostgres_ListPendingRunsCarriesJobName(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	mustCreateRun(t, pg, "team-a/build")
	list, err := pg.ListPendingRuns(ctx, 10, []string{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "team-a/build", list[0].JobName)
}
