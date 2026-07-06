package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupManagedJob registers AppSource "src1" managing Job "hello" with the given spec JSON.
func setupManagedJob(t *testing.T, pg store.Store, srcSpec string) {
	t.Helper()
	ctx := context.Background()
	_, err := pg.UpsertAppSource(ctx, "src1", []byte(srcSpec))
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "src1", "sha", time.Now(),
		[]store.ResourceRef{{Kind: "Job", Name: "hello"}}))
}

const helloJobYAML = `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: hello
spec:
  steps:
    - name: greet
      run: echo hi
`

func applyJob(t *testing.T, s *Server, yaml string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(api.ApplyJobRequest{YAML: yaml})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func TestAPI_ApplyJob_RejectedWhenManaged(t *testing.T) {
	s, pg := newTestServer(t)
	setupManagedJob(t, pg, `{"repoURL":"https://example.com/r.git","targetRevision":"main","path":"jobs"}`)

	rec := applyJob(t, s, helloJobYAML)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `managed by AppSource "src1"`)
	assert.Contains(t, rec.Body.String(), "allowManualOverride")
}

func TestAPI_DeleteJob_RejectedWhenManaged(t *testing.T) {
	s, pg := newTestServer(t)
	_, err := pg.UpsertJob(context.Background(), "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	setupManagedJob(t, pg, `{"repoURL":"https://example.com/r.git","targetRevision":"main","path":"jobs"}`)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/jobs/hello", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	// 拒否されたので行は残っている
	_, err = pg.GetJob(context.Background(), "hello")
	assert.NoError(t, err)
}

func TestAPI_ApplyJob_AllowedWithManualOverride(t *testing.T) {
	s, pg := newTestServer(t)
	setupManagedJob(t, pg,
		`{"repoURL":"https://example.com/r.git","targetRevision":"main","path":"jobs","syncPolicy":{"allowManualOverride":true}}`)

	rec := applyJob(t, s, helloJobYAML)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestAPI_ApplyJob_AllowedWhenUnmanaged(t *testing.T) {
	s, _ := newTestServer(t)
	rec := applyJob(t, s, helloJobYAML)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
