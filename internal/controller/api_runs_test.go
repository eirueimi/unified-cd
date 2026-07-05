package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPI_TriggerRun(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "hello", "unified-cd/v1",
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`))
	body, _ := json.Marshal(api.TriggerRunRequest{JobName: "hello", Params: map[string]string{"k": "v"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var run api.Run
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &run))
	assert.Equal(t, api.RunPending, run.Status)
	assert.Equal(t, "hello", run.JobName)
}

// TestAPI_TriggerRun_ExpandsAgentSelectorParams verifies that the agentSelector template
// `{{ .Params.pool }}` is expanded with the trigger-time params, confirmed via the actual
// ClaimNextRun matching result (without expansion, neither an agent labelled "pool:build"
// nor one carrying the literal string "pool:{{ .Params.pool }}" could claim the run).
func TestAPI_TriggerRun_ExpandsAgentSelectorParams(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "hello", "unified-cd/v1",
		[]byte(`{"agentSelector":["pool:{{ .Params.pool }}"],"steps":[{"name":"s","run":"echo x"}]}`))
	body, _ := json.Marshal(api.TriggerRunRequest{JobName: "hello", Params: map[string]string{"pool": "build"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

	claimed, err := pg.ClaimNextRun(t.Context(), "agent-without-label", []string{"pool:other"})
	require.NoError(t, err)
	assert.Nil(t, claimed, "agent without the expanded label must not claim the run")

	claimed2, err := pg.ClaimNextRun(t.Context(), "agent-with-label", []string{"pool:build"})
	require.NoError(t, err)
	require.NotNil(t, claimed2, "agent with the expanded label must claim the run")
}

func TestAPI_TriggerRun_UnknownJob(t *testing.T) {
	s, _ := newTestServer(t)
	body, _ := json.Marshal(api.TriggerRunRequest{JobName: "missing"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAPI_GetRun(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	r, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+r.ID, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var got api.Run
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, r.ID, got.ID)
}

// TestAPI_GetRun_NotFound verifies FIX #28: a genuinely-missing run still yields
// 404 (via the ErrRunNotFound sentinel). Non-ErrNoRows errors from GetRun now map
// to 500 instead of 404, so a transient DB fault no longer masquerades as "run
// gone" to clients like the k8s pod-GC. The 500 path is documented in the store
// test (it requires injecting a live DB fault to exercise) rather than asserted
// here, but the discrimination lives in handleGetRun (errors.Is check).
func TestAPI_GetRun_NotFound(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/00000000-0000-0000-0000-000000000000", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

func TestAPI_GetRunOutputs_Empty(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	r, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+r.ID+"/outputs", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got api.RunOutputs
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, r.ID, got.RunID)
	assert.Empty(t, got.Outputs)
}

func TestAPI_ListRunsByJob(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "myjob", "unified-cd/v1", []byte(`{"steps":[{"name":"s","run":"echo x"}]}`))
	_, _ = pg.CreateRun(t.Context(), "myjob", nil, []byte(`{}`), nil, "api")
	_, _ = pg.CreateRun(t.Context(), "myjob", nil, []byte(`{}`), nil, "api")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs?jobName=myjob", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var runs []api.Run
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &runs))
	assert.Len(t, runs, 2)
}

func TestAPI_CancelRun(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "api")
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)
	_, _ = pg.ClaimNextRun(t.Context(), "agent-1", nil)
	require.NoError(t, pg.MarkRunRunning(t.Context(), run.ID))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+run.ID+"/cancel", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	got, _ := pg.GetRun(t.Context(), run.ID)
	assert.Equal(t, api.RunCancelled, got.Status)
}

func TestAPI_RunEvents_SSE_ReceivesExistingLogs(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")

	now := time.Now().UTC()
	_, _ = pg.AppendLog(t.Context(), run.ID, 0, "stdout", now, "hello SSE")
	_, _ = pg.AppendLog(t.Context(), run.ID, 0, "stdout", now, "world SSE")

	// Put the Run in Succeeded state so the SSE handler returns immediately.
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)
	_, _ = pg.ClaimNextRun(t.Context(), "agent-1", nil)
	require.NoError(t, pg.MarkRunRunning(t.Context(), run.ID))
	require.NoError(t, pg.MarkRunFinished(t.Context(), run.ID, api.RunSucceeded))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "hello SSE")
	assert.Contains(t, body, "world SSE")
	assert.Contains(t, body, "Succeeded")
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
}

// When the log exceeds the backfill cap, the SSE handler replays only the TAIL
// (the most recent lines, where failures usually are) and emits a "truncated"
// event so the client can say the view is incomplete.
func TestAPI_RunEvents_SSE_BackfillTruncatesToTail(t *testing.T) {
	old := sseBackfillLimit
	sseBackfillLimit = 2
	defer func() { sseBackfillLimit = old }()

	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")
	now := time.Now().UTC()
	for _, ln := range []string{"line-1", "line-2", "line-3", "line-4", "line-5"} {
		_, _ = pg.AppendLog(t.Context(), run.ID, 0, "stdout", now, ln)
	}
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)
	_, _ = pg.ClaimNextRun(t.Context(), "agent-1", nil)
	require.NoError(t, pg.MarkRunRunning(t.Context(), run.ID))
	require.NoError(t, pg.MarkRunFinished(t.Context(), run.ID, api.RunSucceeded))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"type":"truncated"`)
	assert.Contains(t, body, "line-4")
	assert.Contains(t, body, "line-5")
	assert.NotContains(t, body, "line-1")
	assert.NotContains(t, body, "line-2")
	assert.NotContains(t, body, "line-3")
}

func TestAPI_GetRunYAML(t *testing.T) {
	s, pg := newTestServer(t)
	specJSON := []byte(`{"steps":[{"name":"deploy","run":"echo deploy"}]}`)
	_, _ = pg.UpsertJob(t.Context(), "deploy", "unified-cd/v1", specJSON)
	r, _ := pg.CreateRun(t.Context(), "deploy", map[string]string{"env": "prod"}, specJSON, nil, "api")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+r.ID+"/yaml", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Body.String(), "- name: deploy")
	assert.Contains(t, rec.Body.String(), "run: echo deploy")
}

func TestAPI_GetRunYAML_NotFound(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/missing/yaml", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAPI_GetRunYAML_BadSpec(t *testing.T) {
	s, pg := newTestServer(t)
	badSpecJSON := []byte(`{"steps":"broken"}`)
	_, _ = pg.UpsertJob(t.Context(), "broken-run", "unified-cd/v1", badSpecJSON)
	r, _ := pg.CreateRun(t.Context(), "broken-run", nil, badSpecJSON, nil, "api")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+r.ID+"/yaml", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "render yaml: ")
}

func TestAPI_DeleteRun_TerminalState(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")
	require.NoError(t, pg.MarkRunFinished(t.Context(), run.ID, api.RunSucceeded))

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/runs/"+run.ID, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err := pg.GetRun(t.Context(), run.ID)
	assert.Error(t, err, "run should be deleted")
}

func TestAPI_DeleteRun_RejectsNonTerminalState(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")
	// CreateRun creates the Run in Pending state (not a terminal state).

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/runs/"+run.ID, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	got, err := pg.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunPending, got.Status, "run should still exist")
}

func TestAPI_DeleteRun_NotFound(t *testing.T) {
	s, _ := newTestServer(t)
	// A well-formed but absent UUID: GetRun returns ErrNoRows -> ErrRunNotFound -> 404.
	// (A malformed id now surfaces the DB syntax error as 500 rather than a false 404,
	// per FIX #28 — only genuine not-found is 404.)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/runs/00000000-0000-0000-0000-000000000000", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAPI_CancelRun_CascadesToChildRuns(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	parent, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")
	child, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")
	grandchild, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")
	// Link parent→child→grandchild via call: step reports (child_run_id).
	require.NoError(t, pg.UpsertStepReport(t.Context(), parent.ID, 0, 0, "call-child", "", "Running", nil, nil, nil, child.ID, "j"))
	require.NoError(t, pg.UpsertStepReport(t.Context(), child.ID, 0, 0, "call-grandchild", "", "Running", nil, nil, nil, grandchild.ID, "j"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+parent.ID+"/cancel", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	for _, id := range []string{parent.ID, child.ID, grandchild.ID} {
		got, err := pg.GetRun(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, api.RunCancelled, got.Status, "run %s should be cancelled", id)
	}
}

func TestTriggerRun_RecordsPrincipal(t *testing.T) {
	s, pg := newTestServer(t)

	// Create a PAT named "alice" with a known plain token.
	plain := "test-alice-token"
	_, err := pg.CreatePAT(t.Context(), "alice", HashToken(plain), "admin", nil)
	require.NoError(t, err)

	// Create a job to trigger.
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1",
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`))

	body, _ := json.Marshal(api.TriggerRunRequest{JobName: "j"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+plain)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var run api.Run
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &run))
	assert.Equal(t, "alice", run.TriggeredBy)
}

// TestAPI_TriggerRun_MissingRequiredParam verifies that triggering a job with a
// declared `required: true` input and no default fails with 400 when the
// caller omits it, per docs/jobs.md ("the run fails immediately when the
// value is not supplied").
func TestAPI_TriggerRun_MissingRequiredParam(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "needs-image", "unified-cd/v1", []byte(`{
		"params": {"inputs": [{"name": "image", "type": "string", "required": true}]},
		"steps": [{"name": "s", "run": "echo x"}]
	}`))
	body, _ := json.Marshal(api.TriggerRunRequest{JobName: "needs-image"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "image")

	runs, err := pg.ListRunsByJob(t.Context(), "needs-image", 10)
	require.NoError(t, err)
	assert.Empty(t, runs, "no Run should be created when a required param is missing")
}

// TestAPI_TriggerRun_InjectsDefaultParam verifies that an omitted param with a
// declared `default` is injected into the created Run's params.
func TestAPI_TriggerRun_InjectsDefaultParam(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "has-default", "unified-cd/v1", []byte(`{
		"params": {"inputs": [{"name": "tag", "type": "string", "default": "latest"}]},
		"steps": [{"name": "s", "run": "echo x"}]
	}`))
	body, _ := json.Marshal(api.TriggerRunRequest{JobName: "has-default"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var run api.Run
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &run))
	assert.Equal(t, "latest", run.Params["tag"])
}

// TestAPI_TriggerRun_RequiredParamProvided_Succeeds verifies the happy path:
// supplying a required param allows the Run to be created normally.
func TestAPI_TriggerRun_RequiredParamProvided_Succeeds(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "needs-image2", "unified-cd/v1", []byte(`{
		"params": {"inputs": [{"name": "image", "type": "string", "required": true}]},
		"steps": [{"name": "s", "run": "echo x"}]
	}`))
	body, _ := json.Marshal(api.TriggerRunRequest{JobName: "needs-image2", Params: map[string]string{"image": "nginx"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var run api.Run
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &run))
	assert.Equal(t, "nginx", run.Params["image"])
}

func TestAPI_ListActiveRuns(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "myjob", "unified-cd/v1", []byte(`{}`))

	// Create an active Run
	body, _ := json.Marshal(api.TriggerRunRequest{JobName: "myjob"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// Fetch via the active endpoint
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/runs/active", nil)
	req2.Header.Set("Authorization", "Bearer secret")
	rec2 := httptest.NewRecorder()
	s.Router().ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	var runs []api.Run
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &runs))
	require.Len(t, runs, 1)
	assert.Equal(t, "myjob", runs[0].JobName)
}
