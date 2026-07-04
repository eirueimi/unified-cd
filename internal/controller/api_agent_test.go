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
	"github.com/eirueimi/unified-cd/internal/dsl"
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

// TestAgentAPI_Register_RemovesDroppedLabel verifies the TODO #23 fix: re-registering
// an agent with a smaller label set actually removes the dropped label from inventory.
// Before the fix, UpsertAgent used the #12 claim-style DISTINCT-union label merge for
// registration too, so labels could never be removed once seen (audit/inventory lie).
func TestAgentAPI_Register_RemovesDroppedLabel(t *testing.T) {
	s, pg := newTestServer(t)

	body, _ := json.Marshal(api.AgentRegisterRequest{
		AgentID: "a1", Hostname: "host1", OS: "linux",
		Labels: []string{"a", "b"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	body2, _ := json.Marshal(api.AgentRegisterRequest{
		AgentID: "a1", Hostname: "host1", OS: "linux",
		Labels: []string{"a"},
	})
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body2))
	req2.Header.Set("Authorization", "Bearer agent-secret")
	rec2 := httptest.NewRecorder()
	s.Router().ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusNoContent, rec2.Code, rec2.Body.String())

	got, err := pg.GetAgent(context.Background(), "a1")
	require.NoError(t, err)
	require.NotNil(t, got)
	// The register handler always auto-attaches a hostname:<h> label (see
	// TestAgentAPI_Register_DefaultsHostnameLabel), so it's expected alongside "a".
	assert.ElementsMatch(t, []string{"a", "hostname:host1"}, got.Labels, "re-registration must remove dropped label b")
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

// TestAgentAPI_Claim_UpsertsUnregisteredAgent verifies bug #12's fix: an agent that
// never called /register (e.g. because the controller DB was reset out from under a
// still-running agent) still (re)appears in inventory as soon as it claims a run, with
// the labels it presented on the claim request. This closes the "invisible agents run
// jobs" monitoring/audit hole.
func TestAgentAPI_Claim_UpsertsUnregisteredAgent(t *testing.T) {
	s, pg := newTestServer(t)

	// Sanity check: the agent is not present before it ever claims.
	before, err := pg.GetAgent(context.Background(), "ghost-agent")
	require.NoError(t, err)
	assert.Nil(t, before, "agent must not exist before its first claim")

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/ghost-agent/claim?timeout=200ms&labels=kind:linux,zone:us-east", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got, err := pg.GetAgent(context.Background(), "ghost-agent")
	require.NoError(t, err)
	require.NotNil(t, got, "agent must appear in inventory after claiming, even though it never registered")
	assert.Equal(t, "ghost-agent", got.ID)
	assert.ElementsMatch(t, []string{"kind:linux", "zone:us-east"}, got.Labels)

	// Also visible via the list endpoint used by the UI Agents page.
	all, err := pg.ListAgents(context.Background())
	require.NoError(t, err)
	found := false
	for _, a := range all {
		if a.ID == "ghost-agent" {
			found = true
		}
	}
	assert.True(t, found, "ghost-agent must appear in ListAgents")
}

// TestAgentAPI_Claim_DoesNotClobberRegisteredAgent verifies that claims from an agent
// that previously did a full /register call do not wipe out its hostname/OS/version/env,
// even though the claim handler's upsert only knows the agent ID and claim-time labels.
func TestAgentAPI_Claim_DoesNotClobberRegisteredAgent(t *testing.T) {
	s, pg := newTestServer(t)

	regBody, _ := json.Marshal(api.AgentRegisterRequest{
		AgentID: "a1", Hostname: "host1", OS: "linux", Version: "v1.2.3",
		Labels: []string{"kind:linux"},
	})
	regReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(regBody))
	regReq.Header.Set("Authorization", "Bearer agent-secret")
	regRec := httptest.NewRecorder()
	s.Router().ServeHTTP(regRec, regReq)
	require.Equal(t, http.StatusNoContent, regRec.Code, regRec.Body.String())

	claimReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=200ms", nil)
	claimReq.Header.Set("Authorization", "Bearer agent-secret")
	claimRec := httptest.NewRecorder()
	s.Router().ServeHTTP(claimRec, claimReq)
	require.Equal(t, http.StatusOK, claimRec.Code, claimRec.Body.String())

	got, err := pg.GetAgent(context.Background(), "a1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "host1", got.Hostname, "hostname from registration must survive a claim's lightweight upsert")
	assert.Equal(t, "linux", got.OS)
	assert.Equal(t, "v1.2.3", got.Version)
	assert.Contains(t, got.Labels, "kind:linux", "labels from registration must survive a claim's lightweight upsert")
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

func TestBuildClaimResponse_Finally(t *testing.T) {
	spec := dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "build", Run: "make build"},
		},
		Finally: []dsl.StepEntry{
			{Name: "notify", Run: "./notify.sh {{ secrets.HOOK }}"},
			{Name: "rollback", If: "failure()", Run: "./rollback.sh"},
		},
	}
	raw, err := json.Marshal(spec)
	require.NoError(t, err)

	resp, err := buildClaimResponse(&store.ClaimedRun{
		Run:  api.Run{ID: "run1", JobName: "j"},
		Spec: raw,
	})
	require.NoError(t, err)

	require.Len(t, resp.Stages, 1)
	require.Len(t, resp.Finally, 2)
	// Flat step indices continue across steps -> finally.
	assert.Equal(t, 0, resp.Stages[0].Step.Index)
	assert.Equal(t, 1, resp.Finally[0].Step.Index)
	assert.Equal(t, 2, resp.Finally[1].Step.Index)
	assert.Equal(t, "failure()", resp.Finally[1].Step.If)
	// Secrets referenced only in finally are still collected.
	assert.Contains(t, resp.SecretsNeeded, "HOOK")
}

func TestBuildClaimResponse_ApprovalDefaultsTimeout(t *testing.T) {
	spec := dsl.Spec{Steps: []dsl.StepEntry{
		{Name: "gate", Approval: &dsl.ApprovalStep{Message: "ok?"}}, // no timeout
	}}
	raw, _ := json.Marshal(spec)
	resp, err := buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j"}, Spec: raw})
	require.NoError(t, err)
	require.Len(t, resp.Stages, 1)
	require.NotNil(t, resp.Stages[0].Step.Approval)
	assert.Equal(t, "ok?", resp.Stages[0].Step.Approval.Message)
	assert.Equal(t, 60.0, resp.Stages[0].Step.Approval.TimeoutMinutes, "default timeout applied")
}

func TestAgentHeartbeat_TouchesLastSeen(t *testing.T) {
	s, pg := newTestServer(t)
	// register an agent so a row exists
	reg := api.AgentRegisterRequest{AgentID: "agent-hb", Hostname: "h", OS: "linux", Labels: []string{"kind:linux"}}
	body, _ := json.Marshal(reg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	s.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

	before, err := pg.GetAgent(context.Background(), "agent-hb")
	require.NoError(t, err)
	require.NotNil(t, before)

	time.Sleep(10 * time.Millisecond)
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-hb/heartbeat", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	s.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

	after, err := pg.GetAgent(context.Background(), "agent-hb")
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.True(t, after.LastSeenAt.After(before.LastSeenAt),
		"last_seen_at not advanced: before=%v after=%v", before.LastSeenAt, after.LastSeenAt)
}

func TestAgentHeartbeat_RejectsNonAgentToken(t *testing.T) {
	s, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/x/heartbeat", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	s.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusUnauthorized, rr.Code)
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
