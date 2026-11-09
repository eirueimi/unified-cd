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
