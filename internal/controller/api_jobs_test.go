package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestServer creates a Server that mirrors the production main.go flow (syncing UNIFIED_TOKEN
// as a PAT to the DB before starting). "secret" is used for tests under ServerAuth,
// and "agent-secret" is used for tests under BearerAuth(AgentToken).
func newTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	pg := store.NewTestPostgres(t)
	_, err := pg.UpsertBootstrapPAT(context.Background(), "test-bootstrap", HashToken("secret"))
	require.NoError(t, err)
	s := NewServer(Config{AgentToken: "agent-secret"}, pg)
	return s, pg
}

func TestAPI_ApplyJob(t *testing.T) {
	s, _ := newTestServer(t)
	body := api.ApplyJobRequest{YAML: `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: hello
spec:
  steps:
    - name: greet
      run: echo hi
`}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got api.Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "hello", got.Name)
}

func TestAPI_ApplyJob_RejectsInvalidYAML(t *testing.T) {
	s, _ := newTestServer(t)
	body, _ := json.Marshal(api.ApplyJobRequest{YAML: "invalid: yaml: ::"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAPI_GetJob(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "hello", "unified-cd/v1", []byte(`{"steps":[]}`))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/hello", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var got api.Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "hello", got.Name)
}

func TestAPI_ListJobs(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "a", "unified-cd/v1", []byte(`{}`))
	_, _ = pg.UpsertJob(t.Context(), "b", "unified-cd/v1", []byte(`{}`))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var got []api.Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Len(t, got, 2)
}

func TestAPI_GetJob_ReturnsInputs(t *testing.T) {
	s, pg := newTestServer(t)
	specJSON := `{"params":{"inputs":[{"name":"ENV","type":"string","required":true,"description":"Target environment"}]},"steps":[]}`
	_, _ = pg.UpsertJob(t.Context(), "deploy", "unified-cd/v1", []byte(specJSON))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/deploy", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var got api.Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Inputs, 1)
	assert.Equal(t, "ENV", got.Inputs[0].Name)
	assert.Equal(t, "string", got.Inputs[0].Type)
	assert.True(t, got.Inputs[0].Required)
	assert.Equal(t, "Target environment", got.Inputs[0].Description)
}

func TestAPI_ListJobs_ReturnsInputs(t *testing.T) {
	s, pg := newTestServer(t)
	specJSON := `{"params":{"inputs":[{"name":"BRANCH","type":"string"}]},"steps":[]}`
	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1", []byte(specJSON))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var got []api.Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	require.Len(t, got[0].Inputs, 1)
	assert.Equal(t, "BRANCH", got[0].Inputs[0].Name)
}

func TestAPI_GetJob_NoInputsWhenEmpty(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "simple", "unified-cd/v1", []byte(`{"steps":[]}`))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/simple", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var got api.Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Empty(t, got.Inputs)
}

func TestAPI_GetJobYAML(t *testing.T) {
	s, pg := newTestServer(t)
	specJSON := []byte(`{"steps":[{"name":"build","run":"echo hi"}],"params":{"inputs":[{"name":"ENV","type":"string"}]}}`)
	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1", specJSON)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/build/yaml", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Body.String(), "steps:")
	assert.NotContains(t, rec.Body.String(), `"steps"`)
	assert.Contains(t, rec.Body.String(), "- name: build")
	assert.Contains(t, rec.Body.String(), "run: echo hi")
	assert.Contains(t, rec.Body.String(), "inputs:")
}

func TestAPI_GetJobYAML_NotFound(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/missing/yaml", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAPI_GetJobYAML_BadSpec(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "broken", "unified-cd/v1", []byte(`{"steps":"broken"}`))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/broken/yaml", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "render yaml: ")
}

func TestAPI_DeleteJob(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "to-delete", "unified-cd/v1", []byte(`{}`))

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/jobs/to-delete", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err := pg.GetJob(t.Context(), "to-delete")
	assert.Error(t, err, "job should be deleted")
}

// TestAPI_DeleteJob_CascadesRuns verifies that a Job with Run history can be deleted and
// that the associated Runs are also cascade-deleted (migration 014).
func TestAPI_DeleteJob_CascadesRuns(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "to-delete-with-runs", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "to-delete-with-runs", nil, []byte(`{}`), nil, nil, "")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/jobs/to-delete-with-runs", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err := pg.GetRun(t.Context(), run.ID)
	assert.Error(t, err, "run should be cascade-deleted")
}

func TestHandleApplyJob_UsesQualifiedName(t *testing.T) {
	pg := store.NewTestPostgres(t)
	s := &Server{store: pg}
	ctx := context.Background()

	const y = `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
  annotations:
    path: team-a
spec:
  steps:
    - name: c
      run: "true"
`
	require.NoError(t, applyJobYAML(t, s, y))

	job, err := pg.GetJob(ctx, "team-a/build")
	require.NoError(t, err)
	assert.Equal(t, "team-a/build", job.Name)
}

// applyJobYAML mirrors handleApplyJob's parse+store path without going through HTTP,
// so tests can assert on stored state directly.
func applyJobYAML(t *testing.T, s *Server, yaml string) error {
	t.Helper()
	job, err := dsl.Parse(strings.NewReader(yaml))
	if err != nil {
		return err
	}
	_, err = s.storeJob(context.Background(), job)
	return err
}

// TestAPI_ApplyJob_RejectsInvalidPathSegment verifies that a directly-applied
// Job with a traversal-style or otherwise-invalid annotations.path segment is
// rejected with 400 and never reaches the store, while a valid path segment
// still succeeds and produces the expected qualified name.
func TestAPI_ApplyJob_RejectsInvalidPathSegment(t *testing.T) {
	s, pg := newTestServer(t)

	body := api.ApplyJobRequest{YAML: `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: evil
  annotations:
    path: "../evil"
spec:
  steps:
    - name: c
      run: "true"
`}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `invalid path segment "..`)

	// Must not have been stored under any name derived from the bad path.
	_, err := pg.GetJob(t.Context(), "../evil/evil")
	assert.Error(t, err)
	_, err = pg.GetJob(t.Context(), "evil")
	assert.Error(t, err)

	// A segment containing a space is likewise rejected.
	spaceBody := api.ApplyJobRequest{YAML: `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
  annotations:
    path: "a b/%zz"
spec:
  steps:
    - name: c
      run: "true"
`}
	sb, _ := json.Marshal(spaceBody)
	spaceReq := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(sb))
	spaceReq.Header.Set("Authorization", "Bearer secret")
	spaceReq.Header.Set("Content-Type", "application/json")
	spaceRec := httptest.NewRecorder()
	s.Router().ServeHTTP(spaceRec, spaceReq)
	require.Equal(t, http.StatusBadRequest, spaceRec.Code, spaceRec.Body.String())

	// A valid path still works as before.
	okBody := api.ApplyJobRequest{YAML: `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
  annotations:
    path: team-a
spec:
  steps:
    - name: c
      run: "true"
`}
	ob, _ := json.Marshal(okBody)
	okReq := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(ob))
	okReq.Header.Set("Authorization", "Bearer secret")
	okReq.Header.Set("Content-Type", "application/json")
	okRec := httptest.NewRecorder()
	s.Router().ServeHTTP(okRec, okReq)
	require.Equal(t, http.StatusOK, okRec.Code, okRec.Body.String())

	job, err := pg.GetJob(t.Context(), "team-a/build")
	require.NoError(t, err)
	assert.Equal(t, "team-a/build", job.Name)
}

func TestListJobs_DerivesPathAndLeaf(t *testing.T) {
	pg := store.NewTestPostgres(t)
	s := &Server{store: pg}
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "team-a/build", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	jobs, err := s.listJobsDecorated(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "team-a/build", jobs[0].Name)
	assert.Equal(t, "team-a", jobs[0].Path)
	assert.Equal(t, "build", jobs[0].Leaf)
}

func TestExtractJobName(t *testing.T) {
	assert.Equal(t, "team-a/build", extractJobName("team-a/build"))
	assert.Equal(t, "team-a/build", extractJobName("/team-a/build"))

	name, isYAML := extractJobNameAndYAML("team-a/build/yaml")
	assert.Equal(t, "team-a/build", name)
	assert.True(t, isYAML)

	name2, isYAML2 := extractJobNameAndYAML("team-a/build")
	assert.Equal(t, "team-a/build", name2)
	assert.False(t, isYAML2)
}

// TestAPI_GetJob_SlashInName verifies that a hierarchical job name (containing
// "/") is routable via the catch-all "/jobs/*" route end to end, both for the
// job itself and its YAML rendering.
func TestAPI_GetJob_SlashInName(t *testing.T) {
	s, pg := newTestServer(t)
	_, err := pg.UpsertJob(t.Context(), "team-a/build", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/team-a/build", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got api.Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "team-a/build", got.Name)
	assert.Equal(t, "team-a", got.Path)
	assert.Equal(t, "build", got.Leaf)

	yamlReq := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/team-a/build/yaml", nil)
	yamlReq.Header.Set("Authorization", "Bearer secret")
	yamlRec := httptest.NewRecorder()
	s.Router().ServeHTTP(yamlRec, yamlReq)
	require.Equal(t, http.StatusOK, yamlRec.Code, yamlRec.Body.String())
	assert.Equal(t, "text/plain; charset=utf-8", yamlRec.Header().Get("Content-Type"))
}

func TestAPI_GetJob_ReturnsDescription(t *testing.T) {
	s, pg := newTestServer(t)
	specJSON := `{"description":"Deploys the app to production","steps":[]}`
	_, _ = pg.UpsertJob(t.Context(), "deploy", "unified-cd/v1", []byte(specJSON))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/deploy", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got api.Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "Deploys the app to production", got.Description)
}

func TestAPI_ListJobs_ReturnsDescription(t *testing.T) {
	s, pg := newTestServer(t)
	specJSON := `{"description":"Nightly build","steps":[]}`
	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1", []byte(specJSON))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got []api.Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "Nightly build", got[0].Description)
}

func TestAPI_GetJob_NoDescriptionWhenEmpty(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "simple", "unified-cd/v1", []byte(`{"steps":[]}`))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/simple", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got api.Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Empty(t, got.Description)
}
