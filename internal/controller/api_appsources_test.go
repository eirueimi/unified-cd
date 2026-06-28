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
