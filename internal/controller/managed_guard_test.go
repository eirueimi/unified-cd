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

// manageResource marks {kind,name} as managed by AppSource "owner".
func manageResource(t *testing.T, pg store.Store, kind, name string) {
	t.Helper()
	ctx := context.Background()
	_, err := pg.UpsertAppSource(ctx, "owner", []byte(`{"repoURL":"https://example.com/r.git","targetRevision":"main","path":"jobs"}`))
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "owner", "sha", time.Now(),
		[]store.ResourceRef{{Kind: kind, Name: name}}))
}

func TestAPI_DeleteManagedResources_Rejected(t *testing.T) {
	cases := []struct {
		kind, name, path string
	}{
		{"Schedule", "nightly", "/api/v1/schedules/nightly"},
		{"WebhookReceiver", "gh", "/api/v1/webhooks/gh"},
		{"GitCredential", "github", "/api/v1/gitcredentials/github"},
		{"AppSource", "child", "/api/v1/appsources/child"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			s, pg := newTestServer(t)
			manageResource(t, pg, tc.kind, tc.name)
			req := httptest.NewRequest(http.MethodDelete, tc.path, nil)
			req.Header.Set("Authorization", "Bearer secret")
			rec := httptest.NewRecorder()
			s.Router().ServeHTTP(rec, req)
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), `managed by AppSource "owner"`)
		})
	}
}

func TestAPI_ApplySchedule_RejectedWhenManaged(t *testing.T) {
	s, pg := newTestServer(t)
	_, err := pg.UpsertJob(context.Background(), "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	manageResource(t, pg, "Schedule", "nightly")
	b, _ := json.Marshal(api.ApplyScheduleRequest{YAML: `
apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: nightly
spec:
  cron: "0 3 * * *"
  job: hello
`})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/schedules", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

func TestAPI_ApplyWebhook_RejectedWhenManaged(t *testing.T) {
	s, pg := newTestServer(t)
	manageResource(t, pg, "WebhookReceiver", "gh")
	b, _ := json.Marshal(api.ApplyWebhookRequest{YAML: `
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: gh
spec:
  trigger:
    job: hello
  auth:
    type: none
    allowUnauthenticated: true
`})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

func TestAPI_UpsertGitCredential_RejectedWhenManaged(t *testing.T) {
	s, pg := newTestServer(t)
	manageResource(t, pg, "GitCredential", "github")
	b, _ := json.Marshal(api.UpsertGitCredentialRequest{
		Name: "github", Host: "github.com", CredType: "token", SecretRef: "gh-token",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gitcredentials/", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

func TestAPI_ApplyAppSource_RejectedWhenManagedByOther(t *testing.T) {
	s, pg := newTestServer(t)
	manageResource(t, pg, "AppSource", "child")
	b, _ := json.Marshal(api.ApplyAppSourceRequest{YAML: `
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: child
spec:
  repoURL: https://example.com/child.git
  targetRevision: main
  path: jobs
`})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/appsources", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

// TestAPI_ApplyJob_RejectedWhenManaged_RedactsRepoURLCredentials is the
// regression test for I1: errManagedResource.Error() embeds spec.RepoURL raw,
// so a credentialed repoURL (https://user:token@host/repo) managing a Job
// would leak the token straight into the 409 response body. The guard must
// redact it the same way appsource_reconciler.go already does for last_error
// (bug #33), via the shared redactURLCredentials helper.
func TestAPI_ApplyJob_RejectedWhenManaged_RedactsRepoURLCredentials(t *testing.T) {
	s, pg := newTestServer(t)
	setupManagedJob(t, pg, `{"repoURL":"https://user:tok@example.com/r.git","targetRevision":"main","path":"jobs"}`)

	rec := applyJob(t, s, helloJobYAML)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "***")
	assert.NotContains(t, rec.Body.String(), "tok")
}

// app-of-apps: 自分自身をmanaged_resourcesに含むAppSourceのapplyは許可される。
func TestAPI_ApplyAppSource_SelfManagedAllowed(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := context.Background()
	_, err := pg.UpsertAppSource(ctx, "root", []byte(`{"repoURL":"https://example.com/r.git","targetRevision":"main","path":"apps"}`))
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "root", "sha", time.Now(),
		[]store.ResourceRef{{Kind: "AppSource", Name: "root"}}))
	b, _ := json.Marshal(api.ApplyAppSourceRequest{YAML: `
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: root
spec:
  repoURL: https://example.com/r.git
  targetRevision: main
  path: apps
`})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/appsources", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
