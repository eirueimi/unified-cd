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

const testAppSourceYAML = `
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: my-pipelines
spec:
  repoURL: https://github.com/org/repo
  targetRevision: main
  path: jobs/
`

func TestAPI_ApplyAppSource(t *testing.T) {
	s, _ := newTestServer(t)
	body, _ := json.Marshal(api.ApplyAppSourceRequest{YAML: testAppSourceYAML})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/appsources", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var meta api.AppSourceMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &meta))
	assert.Equal(t, "my-pipelines", meta.Name)
	assert.Equal(t, "https://github.com/org/repo", meta.RepoURL)
}

func TestAPI_ApplyAppSource_InvalidYAML(t *testing.T) {
	s, _ := newTestServer(t)
	body, _ := json.Marshal(api.ApplyAppSourceRequest{YAML: "invalid: yaml: ::"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/appsources", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAPI_ListAppSources(t *testing.T) {
	s, pg := newTestServer(t)
	spec := []byte(`{"repoURL":"https://github.com/org/repo","targetRevision":"main","path":"jobs/"}`)
	_, _ = pg.UpsertAppSource(t.Context(), "my-pipelines", spec)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/appsources", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var list []api.AppSourceMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Len(t, list, 1)
}

func TestAPI_GetAppSource(t *testing.T) {
	s, pg := newTestServer(t)
	spec := []byte(`{"repoURL":"https://github.com/org/repo","targetRevision":"main","path":"jobs/"}`)
	_, _ = pg.UpsertAppSource(t.Context(), "my-pipelines", spec)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/appsources/my-pipelines", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var meta api.AppSourceMeta
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &meta))
	assert.Equal(t, "my-pipelines", meta.Name)
}

func TestAPI_DeleteAppSource(t *testing.T) {
	s, pg := newTestServer(t)
	spec := []byte(`{"repoURL":"https://github.com/org/repo","targetRevision":"main","path":"jobs/"}`)
	_, _ = pg.UpsertAppSource(t.Context(), "my-pipelines", spec)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/appsources/my-pipelines", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestAPI_SyncAppSource(t *testing.T) {
	s, pg := newTestServer(t)
	spec := []byte(`{"repoURL":"https://github.com/org/repo","targetRevision":"main","path":"jobs/"}`)
	_, _ = pg.UpsertAppSource(t.Context(), "my-pipelines", spec)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/appsources/my-pipelines/sync", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify that last_commit has been reset to ''.
	got, err := pg.GetAppSource(t.Context(), "my-pipelines")
	require.NoError(t, err)
	assert.Equal(t, "", got.LastCommit)
}

func TestSyncAppSource_SetsSyncingStatus(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := t.Context()
	_, err := pg.UpsertAppSource(ctx, "s1", []byte(`{}`))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/appsources/s1/sync", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)

	got, err := pg.GetAppSource(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, "Syncing", got.SyncStatus)

	// Also verify the GET API response surfaces syncStatus.
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/appsources/s1", nil)
	getReq.Header.Set("Authorization", "Bearer secret")
	getRec := httptest.NewRecorder()
	s.Router().ServeHTTP(getRec, getReq)
	require.Equal(t, http.StatusOK, getRec.Code)
	var meta api.AppSourceMeta
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &meta))
	assert.Equal(t, "Syncing", meta.SyncStatus)
}

func applyAppSourceYAML(t *testing.T, s *Server, yaml string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(api.ApplyAppSourceRequest{YAML: yaml})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/appsources", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func appSourceYAMLWithURL(url string) string {
	return "apiVersion: unified-cd/v1\nkind: AppSource\nmetadata:\n  name: cred-src\nspec:\n  repoURL: " + url + "\n  targetRevision: main\n  path: jobs/\n"
}

func TestAPI_ApplyAppSource_RejectsCredentialedURL(t *testing.T) {
	for _, u := range []string{
		"https://user:s3cr3tpw@github.com/org/repo",
		"https://ghp_s3cr3ttoken@github.com/org/repo",
		"ssh://git:s3cr3tpw@host/org/repo",
	} {
		s, pg := newTestServer(t)
		rec := applyAppSourceYAML(t, s, appSourceYAMLWithURL(u))
		require.Equal(t, http.StatusBadRequest, rec.Code, u)
		assert.Contains(t, rec.Body.String(), "GitCredential")
		assert.NotContains(t, rec.Body.String(), "s3cr3t", "error must not echo the credential")
		list, err := pg.ListAppSources(t.Context())
		require.NoError(t, err)
		assert.Empty(t, list, "rejected AppSource must not be stored")
	}
}

func TestAPI_ApplyAppSource_AllowsSSHUserWithoutPassword(t *testing.T) {
	for _, u := range []string{"ssh://git@host/org/repo.git", "git@github.com:org/repo.git"} {
		s, _ := newTestServer(t)
		rec := applyAppSourceYAML(t, s, appSourceYAMLWithURL(u))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var meta api.AppSourceMeta
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &meta))
		assert.Equal(t, u, meta.RepoURL)
	}
}

// Rows persisted before apply-time rejection existed are redacted on every
// read path (list, get) and in the stored last_error.
func TestAPI_AppSourceOutput_RedactsLegacyCredentials(t *testing.T) {
	s, pg := newTestServer(t)
	spec := []byte(`{"repoURL":"https://user:s3cr3tpw@github.com/org/repo","targetRevision":"main","path":"jobs/"}`)
	_, err := pg.UpsertAppSource(t.Context(), "legacy", spec)
	require.NoError(t, err)
	require.NoError(t, pg.SetAppSourceSyncStatus(t.Context(), "legacy", "Failed", "fetch https://user:s3cr3tpw@github.com/org/repo: denied"))

	for _, path := range []string{"/api/v1/appsources", "/api/v1/appsources/legacy"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, path)
		assert.NotContains(t, rec.Body.String(), "s3cr3t", path)
		assert.Contains(t, rec.Body.String(), "https://github.com/org/repo", path)
	}
}
