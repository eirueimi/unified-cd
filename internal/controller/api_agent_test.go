package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentAPI_Register(t *testing.T) {
	s, _ := newTestServer(t)
	body, _ := json.Marshal(api.AgentRegisterRequest{AgentID: "a1", Hostname: "host1", OS: "linux"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// TestAgentAPI_Register_DefaultsHostnameLabel verifies that a hostname label is automatically
// added at registration so a specific agent can be pinned via agentSelector even when the
// client does not send explicit labels.
func TestAgentAPI_Register_DefaultsHostnameLabel(t *testing.T) {
	s, pg := newTestServer(t)
	body, _ := json.Marshal(api.AgentRegisterRequest{AgentID: "a1", Hostname: "host1", OS: "linux"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	got, err := pg.GetAgent(context.Background(), "a1")
	require.NoError(t, err)
	assert.Contains(t, got.Labels, "hostname:host1")
}

// TestAgentAPI_Register_DoesNotDuplicateExplicitHostnameLabel verifies that when the client
// already specifies a hostname:* label, the server does not add a duplicate.
func TestAgentAPI_Register_DoesNotDuplicateExplicitHostnameLabel(t *testing.T) {
	s, pg := newTestServer(t)
	body, _ := json.Marshal(api.AgentRegisterRequest{
		AgentID: "a1", Hostname: "host1", OS: "linux",
		Labels: []string{"hostname:custom"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	got, err := pg.GetAgent(context.Background(), "a1")
	require.NoError(t, err)
	count := 0
	for _, l := range got.Labels {
		if strings.HasPrefix(l, "hostname:") {
			count++
		}
	}
	assert.Equal(t, 1, count, "hostname label must not be duplicated: %v", got.Labels)
	assert.Contains(t, got.Labels, "hostname:custom")
}

func TestAgentAPI_Claim_EmptyWhenNoQueued(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=200ms", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var got api.ClaimResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Empty(t, got.RunID)
}

func TestAgentAPI_Claim_ReturnsQueuedRun(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1",
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil,
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`), nil, "")
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=2s", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var got api.ClaimResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, run.ID, got.RunID)
	require.Len(t, got.Stages, 1)
	require.NotNil(t, got.Stages[0].Step)
	assert.Equal(t, "echo x", got.Stages[0].Step.Run)
}

func TestAgentAPI_ReportStep(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")
	body, _ := json.Marshal(api.StepReportRequest{
		RunID: run.ID, StepIndex: 0, Status: "Running", StartedAt: time.Now(),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/steps", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func TestAgentAPI_AppendLog(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")
	body, _ := json.Marshal(api.LogAppendRequest{
		RunID: run.ID, StepIndex: 0, Stream: "stdout", Timestamp: time.Now(), Line: "hello",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/logs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	lines, err := pg.TailLogs(context.Background(), run.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, lines, 1)
	assert.Equal(t, "hello", lines[0].Line)
}

func TestAgentAPI_Claim_IncludesOutputsAndCall(t *testing.T) {
	s, pg := newTestServer(t)

	specJSON := []byte(`{
		"params":{"outputs":[{"name":"artifact_url","type":"string"}]},
		"steps":[
			{"name":"build","run":"make build","outputs":{"artifact_url":"{{ .Stdout | grep \"ARTIFACT=\" | cut \"=\" 2 | trim }}"}},
			{"name":"deploy","call":{"job":"deploy-runner","with":{"target":"{{ .Params.env }}"}}}
		]
	}`)
	_, _ = pg.UpsertJob(t.Context(), "multi", "unified-cd/v1", specJSON)
	_, _ = pg.CreateRun(t.Context(), "multi", map[string]string{"env": "prod"}, specJSON, nil, "")
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=2s", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got api.ClaimResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Stages, 2)
	require.NotNil(t, got.Stages[0].Step)
	assert.Equal(t, `{{ .Stdout | grep "ARTIFACT=" | cut "=" 2 | trim }}`, got.Stages[0].Step.Outputs["artifact_url"])
	require.NotNil(t, got.Stages[1].Step)
	require.NotNil(t, got.Stages[1].Step.Call)
	assert.Equal(t, "deploy-runner", got.Stages[1].Step.Call.Job)
	assert.Equal(t, `{{ .Params.env }}`, got.Stages[1].Step.Call.Params["target"])
	assert.Equal(t, []string{"artifact_url"}, got.JobOutputs)
}

func TestAgentAPI_SetStepOutputs(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")

	body, _ := json.Marshal(api.SetOutputsRequest{
		Outputs: map[string]string{"artifact_url": "s3://bucket/a.tar.gz"},
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/a1/runs/"+run.ID+"/steps/0/outputs",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	stored, err := pg.GetStepOutputs(context.Background(), run.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, "s3://bucket/a.tar.gz", stored["artifact_url"])
}

func TestAgentAPI_SetRunOutputs(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")

	body, _ := json.Marshal(api.SetOutputsRequest{
		Outputs: map[string]string{"result": "ok"},
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/a1/runs/"+run.ID+"/outputs",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	stored, err := pg.GetRunOutputs(context.Background(), run.ID)
	require.NoError(t, err)
	assert.Equal(t, "ok", stored["result"])
}

func TestAgentAPI_ClaimResponse_CollectsSecretsNeeded(t *testing.T) {
	s, pg := newTestServer(t)
	specJSON := []byte(`{
		"steps":[
			{"name":"deploy","env":{"AWS_KEY":"{{ secrets.AWS_ACCESS_KEY_ID }}"},"run":"./deploy.sh"},
			{"name":"test","run":"echo {{ secrets.DB_PASS }}"}
		]
	}`)
	_, _ = pg.UpsertJob(t.Context(), "s", "unified-cd/v1", specJSON)
	_, _ = pg.CreateRun(t.Context(), "s", nil, specJSON, nil, "")
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=2s", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got api.ClaimResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.ElementsMatch(t, []string{"AWS_ACCESS_KEY_ID", "DB_PASS"}, got.SecretsNeeded)
	require.NotNil(t, got.Stages[0].Step)
	assert.Equal(t, `{{ secrets.AWS_ACCESS_KEY_ID }}`, got.Stages[0].Step.Env["AWS_KEY"])
}

func TestAgentAPI_LogBulk(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")

	lines := []api.LogAppendRequest{
		{RunID: run.ID, StepIndex: 0, Stream: "stdout", Timestamp: time.Now(), Line: "line1"},
		{RunID: run.ID, StepIndex: 0, Stream: "stdout", Timestamp: time.Now(), Line: "line2"},
		{RunID: run.ID, StepIndex: 0, Stream: "stderr", Timestamp: time.Now(), Line: "err1"},
	}
	body, _ := json.Marshal(lines)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/a1/runs/"+run.ID+"/steps/0/logs/bulk",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	stored, err := pg.TailLogs(context.Background(), run.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, stored, 3)
	assert.Equal(t, "line1", stored[0].Line)
	assert.Equal(t, "line2", stored[1].Line)
	assert.Equal(t, "err1", stored[2].Line)
}

func TestClaimDrainOnShutdown(t *testing.T) {
	s, _ := newTestServer(t)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=60s", nil)
		req.Header.Set("Authorization", "Bearer agent-secret")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		done <- rec
	}()

	time.Sleep(50 * time.Millisecond)
	s.SetShuttingDown()

	select {
	case rec := <-done:
		assert.Equal(t, http.StatusOK, rec.Code)
		var got api.ClaimResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Empty(t, got.RunID, "drain response should have empty RunID")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("claim did not respond within 500ms after SetShuttingDown")
	}
}

func TestBuildClaimResponse_ParallelBlock(t *testing.T) {
	specJSON := []byte(`{
		"steps":[
			{"parallel":[
				{"name":"a","run":"echo a"},
				{"name":"b","run":"echo b"}
			]},
			{"name":"c","run":"echo c"}
		]
	}`)
	claimed := &store.ClaimedRun{
		Run: api.Run{
			ID:      "run-1",
			JobName: "test",
			Params:  map[string]string{},
		},
		Spec: specJSON,
	}
	resp, err := buildClaimResponse(claimed)
	require.NoError(t, err)
	require.Len(t, resp.Stages, 2)
	assert.Nil(t, resp.Stages[0].Step)
	require.Len(t, resp.Stages[0].Parallel, 2)
	assert.Equal(t, "a", resp.Stages[0].Parallel[0].Name)
	assert.Equal(t, 0, resp.Stages[0].Parallel[0].StageIndex)
	assert.Equal(t, 0, resp.Stages[0].Parallel[1].StageIndex)
	require.NotNil(t, resp.Stages[1].Step)
	assert.Equal(t, "c", resp.Stages[1].Step.Name)
	assert.Equal(t, 1, resp.Stages[1].Step.StageIndex)
	assert.Equal(t, 2, resp.Stages[1].Step.Index)
}

func TestClaimDrainBroadcast(t *testing.T) {
	s, _ := newTestServer(t)

	const n = 3
	done := make(chan *httptest.ResponseRecorder, n)
	for range n {
		go func() {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=60s", nil)
			req.Header.Set("Authorization", "Bearer agent-secret")
			rec := httptest.NewRecorder()
			s.Router().ServeHTTP(rec, req)
			done <- rec
		}()
	}

	time.Sleep(100 * time.Millisecond)
	s.SetShuttingDown()

	timeout := time.After(500 * time.Millisecond)
	for i := range n {
		select {
		case rec := <-done:
			assert.Equal(t, http.StatusOK, rec.Code)
			var got api.ClaimResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			assert.Empty(t, got.RunID, "drain response should have empty RunID")
		case <-timeout:
			t.Fatalf("only %d/%d claims were drained", i, n)
		}
	}
}
