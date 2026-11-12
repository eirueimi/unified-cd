package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPI_UpsertGitCredential(t *testing.T) {
	s, _ := newTestServer(t)
	body, _ := json.Marshal(api.UpsertGitCredentialRequest{
		Name: "github-creds", Host: "github.com", CredType: "token", SecretRef: "gh-token",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gitcredentials", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func TestAPI_UpsertGitCredential_RejectsMissingFields(t *testing.T) {
	s, _ := newTestServer(t)
	body, _ := json.Marshal(api.UpsertGitCredentialRequest{Name: "github-creds"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gitcredentials", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "host, credType, secretRef are required")
}

func TestAPI_UpsertGitCredential_RejectsInvalidNameFormat(t *testing.T) {
	s, _ := newTestServer(t)
	body, _ := json.Marshal(api.UpsertGitCredentialRequest{
		Name: "My/Cred", Host: "github.com", CredType: "token", SecretRef: "gh-token",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gitcredentials", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "name is invalid")
}

func TestAPI_UpsertGitCredential_RejectsMissingName(t *testing.T) {
	s, _ := newTestServer(t)
	body, _ := json.Marshal(api.UpsertGitCredentialRequest{
		Host: "github.com", CredType: "token", SecretRef: "gh-token",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gitcredentials", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "name is required")
}

func TestAPI_UpsertGitCredential_KnownHosts(t *testing.T) {
	post := func(r api.UpsertGitCredentialRequest) *httptest.ResponseRecorder {
		s, _ := newTestServer(t)
		body, _ := json.Marshal(r)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/gitcredentials", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec
	}
	const kh = "gitlab.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample"

	rec := post(api.UpsertGitCredentialRequest{Name: "gl", Host: "gitlab.example.com", CredType: "sshKey", SecretRef: "k", KnownHosts: kh})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	rec = post(api.UpsertGitCredentialRequest{Name: "gl", Host: "gitlab.example.com", CredType: "token", SecretRef: "k", KnownHosts: kh})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "knownHosts is only valid")

	rec = post(api.UpsertGitCredentialRequest{Name: "gl", Host: "gitlab.example.com", CredType: "sshKey", SecretRef: "k", KnownHosts: "bogus"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func postGitCredential(t *testing.T, s *Server, req api.UpsertGitCredentialRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/gitcredentials", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, r)
	return rec
}

func listGitCredentials(t *testing.T, s *Server) (string, []api.GitCredentialMeta) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/gitcredentials", nil)
	r.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list []api.GitCredentialMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	return rec.Body.String(), list
}

func gitHubAppRequest(name string) api.UpsertGitCredentialRequest {
	return api.UpsertGitCredentialRequest{
		Name: name, Host: "github.com", CredType: "githubApp", SecretRef: "github-app-key",
		GitHubApp: &api.GitHubAppConfig{AppID: 123456},
		JobAccess: &api.GitJobAccess{
			Folders:      []string{"team-a"},
			Repositories: []string{"acme/app"},
			Permissions:  map[string]string{"contents": "write"},
		},
	}
}

func TestAPI_UpsertGitCredential_GitHubApp(t *testing.T) {
	s, _ := newTestServerWithKM(t)
	const pem = "-----BEGIN RSA PRIVATE KEY-----\nTOPSECRETKEYMATERIAL\n-----END RSA PRIVATE KEY-----"
	mustSetSecret(t, s, "github-app-key", pem)

	req := gitHubAppRequest("gh-app-release")
	rec := postGitCredential(t, s, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	raw, list := listGitCredentials(t, s)
	require.Len(t, list, 1)
	assert.Equal(t, "githubApp", list[0].CredType)
	require.NotNil(t, list[0].GitHubApp)
	assert.Equal(t, int64(123456), list[0].GitHubApp.AppID)
	assert.True(t, list[0].ControllerUse)
	assert.Equal(t, req.JobAccess, list[0].JobAccess)
	assert.NotContains(t, raw, "TOPSECRETKEYMATERIAL")
}

func TestAPI_UpsertGitCredential_GitHubAppValidation(t *testing.T) {
	s, _ := newTestServer(t)
	req := gitHubAppRequest("gh-app-release")
	req.GitHubApp = nil
	rec := postGitCredential(t, s, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "githubApp is required")

	req = gitHubAppRequest("gh-app-release")
	req.JobAccess.Folders = []string{"a/../b"}
	rec = postGitCredential(t, s, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid folder")
}

func TestAPI_UpsertGitCredential_ControllerUseFalse(t *testing.T) {
	s, _ := newTestServer(t)
	f := false
	rec := postGitCredential(t, s, api.UpsertGitCredentialRequest{
		Name: "no-ctl", Host: "github.com", CredType: "token", SecretRef: "gh-token", ControllerUse: &f,
	})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, list := listGitCredentials(t, s)
	require.Len(t, list, 1)
	assert.False(t, list[0].ControllerUse)
}

func TestAPI_UpsertGitCredential_OmittedControllerUseDefaultsTrue(t *testing.T) {
	s, _ := newTestServer(t)
	rec := postGitCredential(t, s, api.UpsertGitCredentialRequest{
		Name: "old-style", Host: "github.com", CredType: "token", SecretRef: "gh-token",
	})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, list := listGitCredentials(t, s)
	require.Len(t, list, 1)
	assert.True(t, list[0].ControllerUse)
}

func TestAPI_GitCredentialNameCollidesWithSecret(t *testing.T) {
	s, _ := newTestServerWithKM(t)
	mustSetSecret(t, s, "gh-app-release", "value")

	rec := postGitCredential(t, s, gitHubAppRequest("gh-app-release"))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `name "gh-app-release" is already used by a StoredSecret; a GitCredential with jobAccess shares the secrets namespace`)

	rec = postGitCredential(t, s, api.UpsertGitCredentialRequest{
		Name: "gh-app-release", Host: "github.com", CredType: "token", SecretRef: "gh-token",
	})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}
