package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const gitHubAppCLIYAML = `apiVersion: unified-cd/v1
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

func TestApply_GitCredentialGitHubAppRequest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gc.yaml")
	require.NoError(t, os.WriteFile(p, []byte(gitHubAppCLIYAML), 0o600))
	tr := &captureTransport{responseFor: func(string) (int, []byte) { return http.StatusNoContent, nil }}
	cmd, _ := newTestApplyCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"-f", p})
	require.NoError(t, cmd.Execute())

	require.Len(t, tr.records, 1)
	assert.Equal(t, "/api/v1/gitcredentials", tr.records[0].path)
	var got api.UpsertGitCredentialRequest
	require.NoError(t, json.Unmarshal(tr.records[0].body, &got))
	yes := true
	assert.Equal(t, api.UpsertGitCredentialRequest{
		Name: "gh-app-release", Host: "github.com", CredType: "githubApp", SecretRef: "github-app-key",
		ControllerUse: &yes,
		GitHubApp:     &api.GitHubAppConfig{AppID: 123456, InstallationID: 7890123, APIURL: "https://api.github.com"},
		JobAccess: &api.GitJobAccess{
			Folders:      []string{"team-a", "platform/tools"},
			Repositories: []string{"acme/app", "acme/infra-*"},
			Permissions:  map[string]string{"contents": "write", "pull_requests": "write"},
		},
	}, got)
}

func TestApply_GitCredentialClientSideValidation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gc.yaml")
	bad := strings.Replace(gitHubAppCLIYAML, "appId: 123456", "appId: 0", 1)
	require.NoError(t, os.WriteFile(p, []byte(bad), 0o600))
	tr := &captureTransport{responseFor: func(string) (int, []byte) { return http.StatusNoContent, nil }}
	cmd, _ := newTestApplyCmd(t, tr, "http://fake")
	cmd.SetArgs([]string{"-f", p})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.githubApp.appId must be a positive integer")
	assert.Empty(t, tr.records)
}

func exportGitCredentials(t *testing.T, metas []api.GitCredentialMeta) string {
	t.Helper()
	tr := &captureTransport{responseFor: func(path string) (int, []byte) {
		if path == "/api/v1/gitcredentials" {
			b, _ := json.Marshal(metas)
			return http.StatusOK, b
		}
		return http.StatusOK, []byte("[]")
	}}
	dir := filepath.Join(t.TempDir(), "out")
	cmd, _ := newTestExportCmd(tr)
	cmd.SetArgs([]string{"-o", dir})
	require.NoError(t, cmd.Execute())
	return dir
}

func TestExport_GitCredentialGitHubApp(t *testing.T) {
	dir := exportGitCredentials(t, []api.GitCredentialMeta{
		{
			Name: "gh-app-release", Host: "github.com", CredType: "githubApp", SecretRef: "github-app-key",
			ControllerUse: false,
			GitHubApp:     &api.GitHubAppConfig{AppID: 123456, InstallationID: 7890123},
			JobAccess: &api.GitJobAccess{
				Folders: []string{"team-a"}, Repositories: []string{"acme/app"},
				Permissions: map[string]string{"contents": "write"},
			},
		},
		{Name: "plain", Host: "github.com", CredType: "token", SecretRef: "gh-token", ControllerUse: true},
	})

	b, err := os.ReadFile(filepath.Join(dir, "gitcredentials", "gh-app-release.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(b), "controllerUse: false")
	gc, err := dsl.ParseGitCredential(bytes.NewReader(b))
	require.NoError(t, err)
	assert.False(t, gc.Spec.ControllerUseOrDefault())
	assert.Equal(t, int64(123456), gc.Spec.GitHubApp.AppID)
	assert.Equal(t, int64(7890123), gc.Spec.GitHubApp.InstallationID)
	assert.Equal(t, []string{"team-a"}, gc.Spec.JobAccess.Folders)
	assert.Equal(t, "write", gc.Spec.JobAccess.Permissions["contents"])

	b, err = os.ReadFile(filepath.Join(dir, "gitcredentials", "plain.yaml"))
	require.NoError(t, err)
	assert.NotContains(t, string(b), "controllerUse")
	_, err = dsl.ParseGitCredential(bytes.NewReader(b))
	require.NoError(t, err)
}

func TestGitCredentialList_ShowsGitHubAppFields(t *testing.T) {
	metas := []api.GitCredentialMeta{
		{
			Name: "gh-app-release", Host: "github.com", CredType: "githubApp", SecretRef: "github-app-key",
			ControllerUse: true,
			GitHubApp:     &api.GitHubAppConfig{AppID: 123456, InstallationID: 7890123},
			JobAccess: &api.GitJobAccess{
				Folders: []string{"team-a"}, Repositories: []string{"acme/app", "acme/infra-*"},
				Permissions: map[string]string{"contents": "write"},
			},
		},
		{
			Name: "ghes", Host: "ghe.example.com", CredType: "githubApp", SecretRef: "k",
			GitHubApp: &api.GitHubAppConfig{AppID: 9, APIURL: "https://ghe.example.com/api/v3"},
		},
	}
	tr := &captureTransport{responseFor: func(string) (int, []byte) {
		b, _ := json.Marshal(metas)
		return http.StatusOK, b
	}}
	prev := http.DefaultClient.Transport
	http.DefaultClient.Transport = tr
	t.Cleanup(func() { http.DefaultClient.Transport = prev })

	root := NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "c.yaml"), "--server", "http://controller.test", "--token", "tok", "gitcredential", "list"})
	require.NoError(t, root.Execute())

	assert.Contains(t, out.String(), "type=githubApp appId=123456 installationId=7890123 controllerUse=true jobAccess=folders:team-a repos:acme/app,acme/infra-* perms:contents=write")
	assert.Contains(t, out.String(), "installationId=auto apiURL=https://ghe.example.com/api/v3 controllerUse=false")
}
