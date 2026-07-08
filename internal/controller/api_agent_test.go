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

// TestAgentAPI_Claim_FailsRunWhenBuildClaimResponseErrors verifies the fix for
// the "stranded Running" bug: when a run's stored spec fails buildClaimResponse
// (e.g. a pre-migration step-level runsIn:), ClaimNextRun has already flipped
// the run to Running in the same SQL statement, and the claiming agent is
// alive and heartbeating — so ListStuckRunIDs' last_seen_at predicate would
// never select it for reaping and it would sit Running forever. The handler
// must instead fail the run immediately (buildClaimResponse errors are
// deterministic, so there is nothing to retry), log the reason on the run,
// and hand the agent an empty claim so it just keeps polling.
func TestAgentAPI_Claim_FailsRunWhenBuildClaimResponseErrors(t *testing.T) {
	s, pg := newTestServer(t)
	specJSON := []byte(`{"steps":[{"name":"compile","run":"go build ./...","runsIn":{"image":"golang:1.22"}}]}`)
	_, _ = pg.UpsertJob(t.Context(), "legacy-job", "unified-cd/v1", specJSON)
	run, _ := pg.CreateRun(t.Context(), "legacy-job", nil, specJSON, nil, "")
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=2s", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var got api.ClaimResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Empty(t, got.RunID, "claim response must be empty so the agent just keeps polling")

	updated, err := pg.GetRun(context.Background(), run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunFailed, updated.Status, "run must be Failed, not left stranded Running")

	lines, err := pg.TailLogs(context.Background(), run.ID, 0, 10)
	require.NoError(t, err)
	require.NotEmpty(t, lines)
	found := false
	for _, l := range lines {
		if l.StepIndex == -1 && l.Stream == "stderr" && strings.Contains(l.Line, "re-apply") {
			found = true
		}
	}
	assert.True(t, found, "expected a stepIndex -1 System log line with the migration hint, got: %+v", lines)

	// Lock release on failure is exercised and asserted by failOrphanedRun's own
	// tests (stuckrun_reaper_test.go); not re-verified here to avoid duplicating
	// that scaffolding.
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

func TestBuildClaimResponse_CarriesScopeFields(t *testing.T) {
	spec := dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "compile", Run: "make", ScopeID: "scope:build", ScopeImage: "golang:1.22"},
			{Parallel: []dsl.Step{
				{Name: "a", Run: "echo a", ScopeID: "scope:par", ScopeImage: "alpine:3"},
				{Name: "b", Run: "echo b"},
			}},
		},
	}
	raw, err := json.Marshal(spec)
	require.NoError(t, err)

	resp, err := buildClaimResponse(&store.ClaimedRun{
		Run:  api.Run{ID: "run1", JobName: "j"},
		Spec: raw,
	})
	require.NoError(t, err)

	require.Len(t, resp.Stages, 2)
	require.NotNil(t, resp.Stages[0].Step)
	assert.Equal(t, "scope:build", resp.Stages[0].Step.ScopeID)
	assert.Equal(t, "golang:1.22", resp.Stages[0].Step.ScopeImage)

	require.Len(t, resp.Stages[1].Parallel, 2)
	assert.Equal(t, "scope:par", resp.Stages[1].Parallel[0].ScopeID)
	assert.Equal(t, "alpine:3", resp.Stages[1].Parallel[0].ScopeImage)
	assert.Equal(t, "", resp.Stages[1].Parallel[1].ScopeID)
	assert.Equal(t, "", resp.Stages[1].Parallel[1].ScopeImage)
}

// TestBuildClaimResponse_ThreadsNative verifies the job-level native: flag
// (dsl.Spec.Native) is threaded onto the ClaimResponse so the agent knows to
// run the claim as a host-process job rather than in a claim pod.
func TestBuildClaimResponse_ThreadsNative(t *testing.T) {
	spec := dsl.Spec{Native: true, Steps: []dsl.StepEntry{{Name: "s", Run: "true"}}}
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	resp, err := buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j"}, Spec: b})
	require.NoError(t, err)
	assert.True(t, resp.Native)
}

// TestBuildClaimResponse_RejectsPreMigrationStepRunsIn verifies a job stored
// before the 2026-07-08 job-isolation release (whose persisted spec JSON
// still carries the removed step-level runsIn: on a non-uses step) fails
// claim building with an actionable error instead of silently dropping the
// field and running the step on the default runner/container.
func TestBuildClaimResponse_RejectsPreMigrationStepRunsIn(t *testing.T) {
	spec := dsl.Spec{Steps: []dsl.StepEntry{
		{Name: "compile", Run: "go build ./...", RunsIn: &dsl.RunsIn{Image: "golang:1.22"}},
	}}
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	_, err = buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "legacy-job"}, Spec: b})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn")
	assert.Contains(t, err.Error(), "re-apply")
}

// TestBuildClaimResponse_RejectsPreMigrationParallelStepRunsIn verifies the
// same guard also walks parallel: sub-steps, not just top-level steps.
func TestBuildClaimResponse_RejectsPreMigrationParallelStepRunsIn(t *testing.T) {
	spec := dsl.Spec{Steps: []dsl.StepEntry{
		{Parallel: []dsl.Step{
			{Name: "a", Run: "echo a"},
			{Name: "b", Run: "echo b", RunsIn: &dsl.RunsIn{Container: "mysql"}},
		}},
	}}
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	_, err = buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "legacy-job"}, Spec: b})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn")
	assert.Contains(t, err.Error(), "re-apply")
}

// TestBuildClaimResponse_RejectsPreMigrationFinallyStepRunsIn verifies the
// guard also walks spec.Finally, not just spec.Steps.
func TestBuildClaimResponse_RejectsPreMigrationFinallyStepRunsIn(t *testing.T) {
	spec := dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "build", Run: "make build"}},
		Finally: []dsl.StepEntry{
			{Name: "cleanup", Run: "make clean", RunsIn: &dsl.RunsIn{Image: "alpine:3"}},
		},
	}
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	_, err = buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "legacy-job"}, Spec: b})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn")
	assert.Contains(t, err.Error(), "re-apply")
}

// TestBuildClaimResponse_UsesStepRunsInStillBuilds verifies a uses: entry's
// runsIn.image (validated at apply time, still legal today) is not
// re-rejected by the pre-migration guard.
func TestBuildClaimResponse_UsesStepRunsInStillBuilds(t *testing.T) {
	spec := dsl.Spec{Steps: []dsl.StepEntry{
		{Name: "build-in-image", Uses: &dsl.UsesStep{Job: "git://example.com/tpl.yaml"}, RunsIn: &dsl.RunsIn{Image: "golang:1.22"}},
	}}
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	resp, err := buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j"}, Spec: b})
	require.NoError(t, err)
	require.Len(t, resp.Stages, 1)
}

// TestBuildClaimResponse_StepContainerThreaded verifies a step's container:
// exec target is threaded onto the ClaimStep (the sole exec-target field on
// the wire type).
func TestBuildClaimResponse_StepContainerThreaded(t *testing.T) {
	spec := dsl.Spec{Steps: []dsl.StepEntry{{Name: "s", Run: "true", Container: "mysql"}}}
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	resp, err := buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j"}, Spec: b})
	require.NoError(t, err)
	require.NotNil(t, resp.Stages[0].Step)
	assert.Equal(t, "mysql", resp.Stages[0].Step.Container)
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

func TestBuildClaimStep_MatrixAndForeachNormalization(t *testing.T) {
	// matrix is converted directly into a dimension list
	entry := dsl.StepEntry{
		Name: "build",
		Run:  "echo",
		Matrix: &dsl.MatrixDef{
			Dimensions: []dsl.MatrixDimension{
				{Name: "os", Source: dsl.ForeachSource{Literal: []string{"linux", "windows"}}},
				{Name: "arch", Source: dsl.ForeachSource{Expr: "$archs"}},
			},
			Exclude: []map[string]string{{"os": "windows"}},
		},
	}
	cs := buildOneClaimStep(0, 0, entry)
	require.NotNil(t, cs.Matrix)
	require.Len(t, cs.Matrix.Dimensions, 2)
	require.Equal(t, "os", cs.Matrix.Dimensions[0].Name)
	require.Equal(t, []string{"linux", "windows"}, cs.Matrix.Dimensions[0].Source.Literal)
	require.Equal(t, "$archs", cs.Matrix.Dimensions[1].Source.Expr)
	require.Equal(t, []map[string]string{{"os": "windows"}}, cs.Matrix.Exclude)

	// foreach is normalized into a single-dimension matrix
	fe := dsl.StepEntry{
		Name:    "deploy",
		Run:     "echo",
		Foreach: &dsl.ForeachDef{Key: "env", Source: dsl.ForeachSource{Literal: []string{"dev", "prod"}}},
	}
	cs = buildOneClaimStep(1, 1, fe)
	require.NotNil(t, cs.Matrix)
	require.Len(t, cs.Matrix.Dimensions, 1)
	require.Equal(t, "env", cs.Matrix.Dimensions[0].Name)
	require.Equal(t, []string{"dev", "prod"}, cs.Matrix.Dimensions[0].Source.Literal)
}

func TestClaimStep_DisplayName(t *testing.T) {
	s := api.ClaimStep{Name: "build"}
	require.Equal(t, "build", s.DisplayName())
	s.MatrixKey = "linux/amd64"
	require.Equal(t, "build (linux, amd64)", s.DisplayName())
}

// TestBuildClaimResponse_ParallelInnerStepMatrixAndForeach verifies that
// foreach/matrix defined on steps inside a `parallel:` block go through the
// same normalization as top-level steps (shared conversion helper). It also
// pins (review finding T5) that a parallel-inner step's Approval field
// survives the dsl.Step -> dsl.StepEntry -> api.ClaimStep conversion chain
// (stepToStepEntry -> buildOneClaimStep) — that piggybacked fix had no
// dedicated assertion before this test was extended.
func TestBuildClaimResponse_ParallelInnerStepMatrixAndForeach(t *testing.T) {
	spec := dsl.Spec{
		Steps: []dsl.StepEntry{
			{Parallel: []dsl.Step{
				{
					Name: "build",
					Run:  "echo",
					Matrix: &dsl.MatrixDef{
						Dimensions: []dsl.MatrixDimension{
							{Name: "os", Source: dsl.ForeachSource{Literal: []string{"linux", "windows"}}},
						},
					},
				},
				{
					Name:    "deploy",
					Run:     "echo",
					Foreach: &dsl.ForeachDef{Key: "env", Source: dsl.ForeachSource{Literal: []string{"dev", "prod"}}},
				},
				{
					Name:     "gate",
					Approval: &dsl.ApprovalStep{Message: "ship it?", TimeoutMinutes: 15},
				},
			}},
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
	require.Len(t, resp.Stages[0].Parallel, 3)

	build := resp.Stages[0].Parallel[0]
	require.NotNil(t, build.Matrix)
	require.Len(t, build.Matrix.Dimensions, 1)
	assert.Equal(t, "os", build.Matrix.Dimensions[0].Name)
	assert.Equal(t, []string{"linux", "windows"}, build.Matrix.Dimensions[0].Source.Literal)

	deploy := resp.Stages[0].Parallel[1]
	require.NotNil(t, deploy.Matrix)
	require.Len(t, deploy.Matrix.Dimensions, 1)
	assert.Equal(t, "env", deploy.Matrix.Dimensions[0].Name)
	assert.Equal(t, []string{"dev", "prod"}, deploy.Matrix.Dimensions[0].Source.Literal)

	gate := resp.Stages[0].Parallel[2]
	require.NotNil(t, gate.Approval, "a parallel-inner step's Approval must survive claim conversion")
	assert.Equal(t, "ship it?", gate.Approval.Message)
	assert.Equal(t, 15.0, gate.Approval.TimeoutMinutes)
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

// TestAgentAPI_FinishRun_FreshTransition verifies the happy path: a first finish
// report on a non-terminal run transitions it and returns 204.
func TestAgentAPI_FinishRun_FreshTransition(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")

	body, _ := json.Marshal(map[string]string{"status": string(api.RunSucceeded)})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/runs/"+run.ID+"/finish", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	got, err := pg.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunSucceeded, got.Status)
}

// TestAgentAPI_FinishRun_FailedCancelsChildren verifies that when a parent run
// (a call: step) is reported Failed, its still-active child runs are cascade
// Cancelled so they don't linger.
func TestAgentAPI_FinishRun_FailedCancelsChildren(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	parent, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")
	child, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")
	// Link the parent's call step to the child run.
	require.NoError(t, pg.UpsertStepReport(t.Context(), parent.ID, 0, 0, "call-child", "", "Running", nil, nil, nil, child.ID, "j"))

	body, _ := json.Marshal(map[string]string{"status": string(api.RunFailed)})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/runs/"+parent.ID+"/finish", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	p, err := pg.GetRun(t.Context(), parent.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunFailed, p.Status)
	c, err := pg.GetRun(t.Context(), child.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunCancelled, c.Status, "child should be cascade-cancelled")
}

// TestAgentAPI_FinishRun_AlreadyTerminal verifies FIX #33: a late finish report on
// an already-terminal run (e.g. the reaper Failed it first) is not falsely
// reported as a fresh success. The handler responds 200 with a body flagging
// alreadyFinalized rather than a plain 204, and does not overwrite the status.
func TestAgentAPI_FinishRun_AlreadyTerminal(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")
	// Simulate the reaper finalizing the run as Failed before the agent's late report.
	require.NoError(t, pg.MarkRunFinished(t.Context(), run.ID, api.RunFailed))

	body, _ := json.Marshal(map[string]string{"status": string(api.RunSucceeded)})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/runs/"+run.ID+"/finish", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["alreadyFinalized"])

	got, err := pg.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunFailed, got.Status, "the reaper's terminal status must not be overwritten")
}

// TestAgentAPI_ReportStep_AlreadyTerminal verifies the step-report guard: a late
// step report under an already-terminal run does not write stale step state and
// is reported distinctly (200-with-body) instead of a 204 that would suggest the
// write happened.
func TestAgentAPI_ReportStep_AlreadyTerminal(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")
	require.NoError(t, pg.MarkRunFinished(t.Context(), run.ID, api.RunFailed))

	body, _ := json.Marshal(api.StepReportRequest{
		RunID: run.ID, StepIndex: 0, Status: "Succeeded", StartedAt: time.Now(), EndedAt: time.Now(),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/steps", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["alreadyFinalized"])

	steps, err := pg.GetRunSteps(t.Context(), run.ID)
	require.NoError(t, err)
	assert.Empty(t, steps, "no step report should be written under an already-terminal run")
}
