package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store/storetest"
)

func TestApplyResource_EachKind(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	ctx := context.Background()
	cases := []struct {
		kind, wantName, doc string
	}{
		{"Job", "j1", "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j1\nspec:\n  agentSelector: [kind:docker]\n  steps:\n    - name: s\n      run: echo hi"},
		{"Schedule", "sc1", "apiVersion: unified-cd/v1\nkind: Schedule\nmetadata:\n  name: sc1\nspec:\n  cron: \"* * * * *\"\n  job: j1"},
		{"WebhookReceiver", "wh1", "apiVersion: unified-cd/v1\nkind: WebhookReceiver\nmetadata:\n  name: wh1\nspec:\n  trigger:\n    job: j1\n  auth:\n    type: none\n    allowUnauthenticated: true"},
		{"GitCredential", "gc1", "apiVersion: unified-cd/v1\nkind: GitCredential\nmetadata:\n  name: gc1\nspec:\n  host: github.com\n  type: token\n  secretRef: s"},
		{"AppSource", "as1", "apiVersion: unified-cd/v1\nkind: AppSource\nmetadata:\n  name: as1\nspec:\n  repoURL: https://x/y\n  targetRevision: main\n  path: jobs"},
	}
	for _, c := range cases {
		got, err := applyResource(ctx, pg, c.kind, "", []byte(c.doc))
		if err != nil {
			t.Fatalf("%s: applyResource error: %v", c.kind, err)
		}
		if got != c.wantName {
			t.Errorf("%s: name = %q, want %q", c.kind, got, c.wantName)
		}
	}
}

func TestApplyResource_UnknownAndBad(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	ctx := context.Background()
	_, err := applyResource(ctx, pg, "Nope", "", []byte("kind: Nope"))
	if err == nil {
		t.Error("unknown kind: expected error")
	}
	require.False(t, errors.Is(err, errStoreWrite), "unknown kind error must not be classified as a store-write failure")

	_, err = applyResource(ctx, pg, "Job", "", []byte("kind: Job\nmetadata: {name: x}\nspec: {steps: []}"))
	if err == nil {
		t.Error("invalid Job: expected error")
	}
	require.False(t, errors.Is(err, errStoreWrite), "bad-parse error must not be classified as a store-write failure")
}

func TestProbeKind(t *testing.T) {
	if k := probeKind([]byte("kind: Schedule\nmetadata: {name: x}")); k != "Schedule" {
		t.Errorf("probeKind = %q, want Schedule", k)
	}
	if k := probeKind([]byte("metadata: {name: x}")); k != "" {
		t.Errorf("probeKind (no kind) = %q, want empty", k)
	}
}

const gitHubAppApplyDoc = `apiVersion: unified-cd/v1
kind: GitCredential
metadata:
  name: gh-app-release
spec:
  host: github.com
  type: githubApp
  secretRef: github-app-key
  controllerUse: false
  githubApp:
    appId: 123456
    installationId: 7890123
  jobAccess:
    folders: [team-a, /platform/tools/, "*"]
    repositories: [acme/app, acme/infra-*]
    permissions:
      contents: write
`

func TestApplyResource_GitCredentialGitHubApp(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	ctx := context.Background()
	name, err := applyResource(ctx, pg, "GitCredential", "", []byte(gitHubAppApplyDoc))
	require.NoError(t, err)
	require.Equal(t, "gh-app-release", name)

	got, err := pg.GetGitCredential(ctx, "gh-app-release")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "githubApp", got.CredType)
	require.False(t, got.ControllerUse)
	require.Equal(t, &api.GitHubAppConfig{AppID: 123456, InstallationID: 7890123}, got.GitHubApp)
	require.Equal(t, &api.GitJobAccess{
		Folders:      []string{"team-a", "platform/tools", "*"},
		Repositories: []string{"acme/app", "acme/infra-*"},
		Permissions:  map[string]string{"contents": "write"},
	}, got.JobAccess)

	// Collision with a StoredSecret of the same name is rejected.
	_, err = pg.UpsertSecret(ctx, "taken", []byte("dek"), []byte("ct"))
	require.NoError(t, err)
	doc := strings.Replace(gitHubAppApplyDoc, "gh-app-release", "taken", 1)
	_, err = applyResource(ctx, pg, "GitCredential", "", []byte(doc))
	require.Error(t, err)
	require.Contains(t, err.Error(), "already used by a StoredSecret")
	require.False(t, errors.Is(err, errStoreWrite), "a name collision is a manifest problem, not a store failure")
}

func TestApplyResource_GitCredentialGitHubAppKeyPairingRejected(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	ctx := context.Background()
	_, err := applyResource(ctx, pg, "GitCredential", "", []byte(gitHubAppApplyDoc))
	require.NoError(t, err)

	tokenDoc := `apiVersion: unified-cd/v1
kind: GitCredential
metadata:
  name: exfil
spec:
  host: attacker.example
  type: token
  secretRef: github-app-key
`
	_, err = applyResource(ctx, pg, "GitCredential", "", []byte(tokenDoc))
	require.Error(t, err)
	require.Contains(t, err.Error(), `secretRef "github-app-key" is the private key of GitHub App credential "gh-app-release"; a token or sshKey GitCredential cannot use it`)
	require.True(t, errors.Is(err, errNameCollision))
	require.False(t, errors.Is(err, errStoreWrite))

	// The reverse: a githubApp credential naming a token credential's secret.
	sshDoc := strings.NewReplacer("name: exfil", "name: deploy", "type: token", "type: sshKey", "secretRef: github-app-key", "secretRef: deploy-key").Replace(tokenDoc)
	_, err = applyResource(ctx, pg, "GitCredential", "", []byte(sshDoc))
	require.NoError(t, err)
	appDoc := strings.NewReplacer("name: gh-app-release", "name: gh-app-2", "secretRef: github-app-key", "secretRef: deploy-key").Replace(gitHubAppApplyDoc)
	_, err = applyResource(ctx, pg, "GitCredential", "", []byte(appDoc))
	require.Error(t, err)
	require.Contains(t, err.Error(), `secretRef "deploy-key" is already used by sshKey GitCredential "deploy"; a GitHub App private key cannot be shared with a token or sshKey GitCredential`)
	require.True(t, errors.Is(err, errNameCollision))
}
