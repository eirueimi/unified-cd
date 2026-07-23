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
	"github.com/eirueimi/unified-cd/internal/gittemplate"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// claimRunForTest transitions run to Queued and claims it with agentID via
// the store — the same store-level sequence api_agent_reconcile_test.go uses
// — leaving it Running with ClaimedBy == agentID. Agent write handlers now
// enforce run ownership (agentRunGuard), so any pre-existing test that posts
// as a fixed agent ID must claim the run with that same ID first, or every
// write it sends is a stranger's write and gets 403'd.
func claimRunForTest(t *testing.T, pg store.Store, agentID, runID string) {
	t.Helper()
	_, err := pg.TransitionPendingToQueued(t.Context(), 10)
	require.NoError(t, err)
	claimed, err := pg.ClaimNextRun(t.Context(), agentID, nil)
	require.NoError(t, err)
	require.Equal(t, runID, claimed.ID)
}

func TestAgentAPI_Register(t *testing.T) {
	s, st := newTestServer(t)
	token := issueAgentAccessForTest(t, st, "a1", nil, nil)
	body, _ := json.Marshal(api.AgentRegisterRequest{AgentID: "a1", Hostname: "host1", OS: "linux"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// TestAgentAPI_Register_RemovesDroppedLabel verifies the TODO #23 fix: re-registering
// an agent with a smaller label set actually removes the dropped label from inventory.
// Before the fix, UpsertAgent used the #12 claim-style DISTINCT-union label merge for
// registration too, so labels could never be removed once seen (audit/inventory lie).
//
// Migrated off the legacy bearer: an enrolled principal's registered labels
// always come from its credential's authorized set, not the self-reported
// request body (handleAgentRegister overwrites req.Labels for any non-legacy
// principal), so "a smaller label set" here means re-minting the agent's
// credential with a smaller authorized set — mirroring an admin re-issuing
// it — rather than sending a smaller Labels field. No hostname:<h> label is
// expected either: that synthesis is legacy-only (see
// TestAgentAPI_Register_DefaultsHostnameLabel above).
func TestAgentAPI_Register_RemovesDroppedLabel(t *testing.T) {
	s, pg := newTestServer(t)

	token1 := issueAgentAccessForTest(t, pg, "a1", []string{"a", "b"}, nil)
	body, _ := json.Marshal(api.AgentRegisterRequest{AgentID: "a1", Hostname: "host1", OS: "linux"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token1)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// Re-mint with a smaller authorized set and register again.
	token2 := issueAgentAccessForTest(t, pg, "a1", []string{"a"}, nil)
	body2, _ := json.Marshal(api.AgentRegisterRequest{AgentID: "a1", Hostname: "host1", OS: "linux"})
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body2))
	req2.Header.Set("Authorization", "Bearer "+token2)
	rec2 := httptest.NewRecorder()
	s.Router().ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusNoContent, rec2.Code, rec2.Body.String())

	got, err := pg.GetAgent(context.Background(), "a1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.ElementsMatch(t, []string{"a"}, got.Labels, "re-registration must remove dropped label b")
}

func TestAgentAPI_Claim_EmptyWhenNoQueued(t *testing.T) {
	s, st := newTestServer(t)
	token := issueAgentAccessForTest(t, st, "a1", nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=200ms", nil)
	req.Header.Set("Authorization", "Bearer "+token)
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
		[]byte(`{"steps":[{"name":"s","run":"echo x"}]}`), nil, nil, "")
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=2s", nil)
	req.Header.Set("Authorization", "Bearer "+token)
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
// its labels. This closes the "invisible agents run jobs" monitoring/audit hole.
//
// Migrated off the legacy bearer: handleAgentClaim only parses the `?labels=`
// query param for a legacy principal; an enrolled/uca_ principal's claim-time
// labels always come from the credential's authorized set instead
// (api_agent.go), so the labels are attached to the minted token, not the
// query string.
func TestAgentAPI_Claim_UpsertsUnregisteredAgent(t *testing.T) {
	s, pg := newTestServer(t)

	// Sanity check: the agent is not present before it ever claims.
	before, err := pg.GetAgent(context.Background(), "ghost-agent")
	require.NoError(t, err)
	assert.Nil(t, before, "agent must not exist before its first claim")

	token := issueAgentAccessForTest(t, pg, "ghost-agent", []string{"kind:linux", "zone:us-east"}, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/ghost-agent/claim?timeout=200ms", nil)
	req.Header.Set("Authorization", "Bearer "+token)
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

	// A single credential authorized for "kind:linux" backs both the register
	// and the claim call below, matching what a real enrolled agent presents
	// to both routes.
	token := issueAgentAccessForTest(t, pg, "a1", []string{"kind:linux"}, nil)

	regBody, _ := json.Marshal(api.AgentRegisterRequest{
		AgentID: "a1", Hostname: "host1", OS: "linux", Version: "v1.2.3",
	})
	regReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(regBody))
	regReq.Header.Set("Authorization", "Bearer "+token)
	regRec := httptest.NewRecorder()
	s.Router().ServeHTTP(regRec, regReq)
	require.Equal(t, http.StatusNoContent, regRec.Code, regRec.Body.String())

	claimReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=200ms", nil)
	claimReq.Header.Set("Authorization", "Bearer "+token)
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
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a1", run.ID)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)
	body, _ := json.Marshal(api.StepReportRequest{
		RunID: run.ID, StepIndex: 0, Status: "Running", StartedAt: time.Now(),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/steps", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func TestAgentAPI_AppendLog(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a1", run.ID)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)
	body, _ := json.Marshal(api.LogAppendRequest{
		RunID: run.ID, StepIndex: 0, Stream: "stdout", Timestamp: time.Now(), Line: "hello",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/logs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
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
	_, _ = pg.CreateRun(t.Context(), "multi", map[string]string{"env": "prod"}, specJSON, nil, nil, "")
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=2s", nil)
	req.Header.Set("Authorization", "Bearer "+token)
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
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a1", run.ID)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	body, _ := json.Marshal(api.SetOutputsRequest{
		Outputs: map[string]string{"artifact_url": "s3://bucket/a.tar.gz"},
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/a1/runs/"+run.ID+"/steps/0/outputs",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
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
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a1", run.ID)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	body, _ := json.Marshal(api.SetOutputsRequest{
		Outputs: map[string]string{"result": "ok"},
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/a1/runs/"+run.ID+"/outputs",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
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
	_, _ = pg.CreateRun(t.Context(), "s", nil, specJSON, nil, nil, "")
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=2s", nil)
	req.Header.Set("Authorization", "Bearer "+token)
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
	run, _ := pg.CreateRun(t.Context(), "legacy-job", nil, specJSON, nil, nil, "")
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=2s", nil)
	req.Header.Set("Authorization", "Bearer "+token)
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
		if l.StepIndex == -1 && l.Stream == "stderr" && strings.Contains(l.Line, "container:") {
			found = true
		}
	}
	assert.True(t, found, "expected a stepIndex -1 System log line with the runsIn guidance, got: %+v", lines)

	// Lock release on failure is exercised and asserted by failOrphanedRun's own
	// tests (stuckrun_reaper_test.go); not re-verified here to avoid duplicating
	// that scaffolding.
}

func TestAgentAPI_LogBulk(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a1", run.ID)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	lines := []api.LogAppendRequest{
		{RunID: run.ID, StepIndex: 0, Stream: "stdout", Timestamp: time.Now(), Line: "line1"},
		{RunID: run.ID, StepIndex: 0, Stream: "stdout", Timestamp: time.Now(), Line: "line2"},
		{RunID: run.ID, StepIndex: 0, Stream: "stderr", Timestamp: time.Now(), Line: "err1"},
	}
	body, _ := json.Marshal(lines)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/a1/runs/"+run.ID+"/steps/0/logs/bulk",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
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

// TestAgentAPI_LogBulk_MixedOwnershipRejectsWholeBatch pins the guard-loop
// hoist in handleAgentLogBulk: guards run in a pass over the distinct
// RunIDs BEFORE any line is appended, so a batch straddling an owned run
// and a run claimed by another agent is rejected in full — zero lines land
// for either run — rather than the owned run's lines landing before the
// not-owned run's line is discovered mid-loop.
func TestAgentAPI_LogBulk_MixedOwnershipRejectsWholeBatch(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))

	ownedRun, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a1", ownedRun.ID)

	otherRun, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a2", otherRun.ID)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	lines := []api.LogAppendRequest{
		{RunID: ownedRun.ID, StepIndex: 0, Stream: "stdout", Timestamp: time.Now(), Line: "line1"},
		{RunID: otherRun.ID, StepIndex: 0, Stream: "stdout", Timestamp: time.Now(), Line: "line2"},
	}
	body, _ := json.Marshal(lines)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/a1/runs/"+ownedRun.ID+"/steps/0/logs/bulk",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	ownedStored, err := pg.TailLogs(context.Background(), ownedRun.ID, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, ownedStored, "owned run's line must not land when the batch is rejected")

	otherStored, err := pg.TailLogs(context.Background(), otherRun.ID, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, otherStored, "not-owned run's line must not land")
}

func TestClaimDrainOnShutdown(t *testing.T) {
	s, st := newTestServer(t)
	token := issueAgentAccessForTest(t, st, "a1", nil, nil)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=60s", nil)
		req.Header.Set("Authorization", "Bearer "+token)
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
	assert.Contains(t, err.Error(), "container:")
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
	assert.Contains(t, err.Error(), "container:")
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
	assert.Contains(t, err.Error(), "container:")
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
	token := issueAgentAccessForTest(t, pg, "agent-hb", []string{"kind:linux"}, nil)
	// register an agent so a row exists
	reg := api.AgentRegisterRequest{AgentID: "agent-hb", Hostname: "h", OS: "linux"}
	body, _ := json.Marshal(reg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	s.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

	before, err := pg.GetAgent(context.Background(), "agent-hb")
	require.NoError(t, err)
	require.NotNil(t, before)

	time.Sleep(10 * time.Millisecond)
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-hb/heartbeat", nil)
	req.Header.Set("Authorization", "Bearer "+token)
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

// TestAgentHeartbeat_ReconcilesLostClaims verifies that a heartbeat body
// reporting an agent's active run set causes the controller to fail any
// Running run it claimed to that agent which is both absent from the
// reported set and has sat claimed past the reconcile grace window — while
// leaving reported runs and runs claimed within grace alone.
func TestAgentHeartbeat_ReconcilesLostClaims(t *testing.T) {
	s, pg := newTestServer(t)
	pgc, ok := pg.(*store.Postgres)
	require.True(t, ok, "test relies on concrete *store.Postgres for claimed_at backdating")
	ctx := context.Background()

	_, err := pgc.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	r1, err := pgc.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	r2, err := pgc.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	r3, err := pgc.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	_, err = pgc.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)

	// Claim all three onto the same agent (FIFO claim order matches creation order).
	for _, want := range []string{r1.ID, r2.ID, r3.ID} {
		claimed, err := pgc.ClaimNextRun(ctx, "agent-hb-reconcile", nil)
		require.NoError(t, err)
		require.Equal(t, want, claimed.ID)
	}

	// r1 and r3 were claimed "5 minutes ago" (past the 60s grace); r2 keeps
	// its natural just-claimed timestamp (well within grace).
	require.NoError(t, pgc.BackdateRunClaimedAt(ctx, r1.ID, 5*time.Minute))
	require.NoError(t, pgc.BackdateRunClaimedAt(ctx, r3.ID, 5*time.Minute))

	body, err := json.Marshal(api.HeartbeatRequest{ActiveRunIDs: []string{r1.ID}})
	require.NoError(t, err)
	token := issueAgentAccessForTest(t, pg, "agent-hb-reconcile", nil, nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-hb-reconcile/heartbeat", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

	got, err := pgc.GetRun(ctx, r1.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunRunning, got.Status, "r1 was reported active: must stay Running")

	got, err = pgc.GetRun(ctx, r2.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunRunning, got.Status, "r2 is within the grace window: must stay Running")

	got, err = pgc.GetRun(ctx, r3.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunFailed, got.Status, "r3 is stale and unreported: must be Failed")
}

// TestAgentHeartbeat_ReconcileEmptyActiveSetStillFails pins the correctness
// fix flagged in Task 3's review: a LIVE agent reporting zero active runs
// still sends a body (`{"activeRunIds":[]}`), and that body must still
// trigger reconcile — gating on r.ContentLength != 0 (body presence), not
// on the decoded slice being non-nil, is what makes this work.
func TestAgentHeartbeat_ReconcileEmptyActiveSetStillFails(t *testing.T) {
	s, pg := newTestServer(t)
	pgc, ok := pg.(*store.Postgres)
	require.True(t, ok, "test relies on concrete *store.Postgres for claimed_at backdating")
	ctx := context.Background()

	_, err := pgc.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	r4, err := pgc.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	_, err = pgc.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	claimed, err := pgc.ClaimNextRun(ctx, "agent-hb-empty", nil)
	require.NoError(t, err)
	require.Equal(t, r4.ID, claimed.ID)
	require.NoError(t, pgc.BackdateRunClaimedAt(ctx, r4.ID, 5*time.Minute))

	body, err := json.Marshal(api.HeartbeatRequest{ActiveRunIDs: []string{}})
	require.NoError(t, err)
	require.Equal(t, `{"activeRunIds":[]}`, string(body), "no omitempty: an empty set must still marshal a body")
	token := issueAgentAccessForTest(t, pg, "agent-hb-empty", nil, nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-hb-empty/heartbeat", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

	got, err := pgc.GetRun(ctx, r4.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunFailed, got.Status, "empty-but-present active set must still reconcile stale runs")
}

// TestAgentHeartbeat_BodylessSkipsReconcile pins backward compatibility: a
// legacy agent sends no heartbeat body at all (ContentLength == 0), and that
// must never trigger reconcile — the controller cannot distinguish "no
// active runs" from "doesn't know how to report them" without a body.
func TestAgentHeartbeat_BodylessSkipsReconcile(t *testing.T) {
	s, pg := newTestServer(t)
	pgc, ok := pg.(*store.Postgres)
	require.True(t, ok, "test relies on concrete *store.Postgres for claimed_at backdating")
	ctx := context.Background()

	_, err := pgc.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	r5, err := pgc.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	_, err = pgc.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	claimed, err := pgc.ClaimNextRun(ctx, "agent-hb-legacy", nil)
	require.NoError(t, err)
	require.Equal(t, r5.ID, claimed.ID)
	require.NoError(t, pgc.BackdateRunClaimedAt(ctx, r5.ID, 5*time.Minute))

	token := issueAgentAccessForTest(t, pg, "agent-hb-legacy", nil, nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-hb-legacy/heartbeat", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	s.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

	got, err := pgc.GetRun(ctx, r5.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunRunning, got.Status, "bodyless heartbeat must skip reconcile entirely")
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
	cs := buildOneClaimStep(0, 0, entry, nil)
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
	cs = buildOneClaimStep(1, 1, fe, nil)
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
	s, st := newTestServer(t)
	token := issueAgentAccessForTest(t, st, "a1", nil, nil)

	const n = 3
	done := make(chan *httptest.ResponseRecorder, n)
	for range n {
		go func() {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=60s", nil)
			req.Header.Set("Authorization", "Bearer "+token)
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
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a1", run.ID)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	body, _ := json.Marshal(map[string]string{"status": string(api.RunSucceeded)})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/runs/"+run.ID+"/finish", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
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
	parent, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a1", parent.ID)
	child, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	// Link the parent's call step to the child run.
	require.NoError(t, pg.UpsertStepReport(t.Context(), parent.ID, 0, 0, "call-child", "", "Running", nil, nil, nil, child.ID, "j"))
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	body, _ := json.Marshal(map[string]string{"status": string(api.RunFailed)})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/runs/"+parent.ID+"/finish", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
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
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a1", run.ID)
	// Simulate the reaper finalizing the run as Failed before the agent's late report.
	require.NoError(t, pg.MarkRunFinished(t.Context(), run.ID, api.RunFailed))
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	body, _ := json.Marshal(map[string]string{"status": string(api.RunSucceeded)})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/runs/"+run.ID+"/finish", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
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
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a1", run.ID)
	require.NoError(t, pg.MarkRunFinished(t.Context(), run.ID, api.RunFailed))
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	body, _ := json.Marshal(api.StepReportRequest{
		RunID: run.ID, StepIndex: 0, Status: "Succeeded", StartedAt: time.Now(), EndedAt: time.Now(),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/steps", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
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

// TestBuildClaimResponse_Shell_JobLevelDefaultApplied verifies a step with no
// shell: of its own inherits the job-level spec.shell.
func TestBuildClaimResponse_Shell_JobLevelDefaultApplied(t *testing.T) {
	spec := dsl.Spec{
		Shell: []string{"bash", "-lc"},
		Steps: []dsl.StepEntry{{Name: "build", Run: "make"}},
	}
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	resp, err := buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j"}, Spec: b})
	require.NoError(t, err)
	require.NotNil(t, resp.Stages[0].Step)
	assert.Equal(t, []string{"bash", "-lc"}, resp.Stages[0].Step.Shell)
}

// TestBuildClaimResponse_Shell_StepOverrideWins verifies a step-level shell:
// takes priority over the job-level spec.shell.
func TestBuildClaimResponse_Shell_StepOverrideWins(t *testing.T) {
	spec := dsl.Spec{
		Shell: []string{"bash", "-lc"},
		Steps: []dsl.StepEntry{{Name: "quick", Run: "print('hi')", Shell: []string{"sh", "-c"}}},
	}
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	resp, err := buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j"}, Spec: b})
	require.NoError(t, err)
	require.NotNil(t, resp.Stages[0].Step)
	assert.Equal(t, []string{"sh", "-c"}, resp.Stages[0].Step.Shell)
}

// TestBuildClaimResponse_Shell_NilWhenNeitherDeclared verifies a bare step
// under a bare job carries a nil Shell — "agent applies the shim default".
func TestBuildClaimResponse_Shell_NilWhenNeitherDeclared(t *testing.T) {
	spec := dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "bare", Run: "echo hi"}},
	}
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	resp, err := buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j"}, Spec: b})
	require.NoError(t, err)
	require.NotNil(t, resp.Stages[0].Step)
	assert.Nil(t, resp.Stages[0].Step.Shell)
}

// TestBuildClaimResponse_Shell_FinallyAndParallelCovered verifies the same
// resolution applies inside parallel: blocks and finally:.
func TestBuildClaimResponse_Shell_FinallyAndParallelCovered(t *testing.T) {
	spec := dsl.Spec{
		Shell: []string{"bash", "-lc"},
		Steps: []dsl.StepEntry{
			{Parallel: []dsl.Step{
				{Name: "a", Run: "echo a", Shell: []string{"python3", "-c"}},
				{Name: "b", Run: "echo b"},
			}},
		},
		Finally: []dsl.StepEntry{
			{Name: "cleanup", Run: "echo cleanup"},
			{Name: "notify", Run: "echo notify", Shell: []string{"sh", "-c"}},
		},
	}
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	resp, err := buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j"}, Spec: b})
	require.NoError(t, err)

	require.Len(t, resp.Stages[0].Parallel, 2)
	assert.Equal(t, []string{"python3", "-c"}, resp.Stages[0].Parallel[0].Shell, "parallel step override wins")
	assert.Equal(t, []string{"bash", "-lc"}, resp.Stages[0].Parallel[1].Shell, "parallel step inherits job default")

	require.Len(t, resp.Finally, 2)
	assert.Equal(t, []string{"bash", "-lc"}, resp.Finally[0].Step.Shell, "finally step inherits job default")
	assert.Equal(t, []string{"sh", "-c"}, resp.Finally[1].Step.Shell, "finally step override wins")
}

// TestBuildClaimResponse_Shell_PostCarriedOnlyWhenDeclared verifies post.Shell
// is copied through as-is: present when the dsl post declares one, nil
// otherwise (nil signals the agent should inherit the owning step's Shell).
func TestBuildClaimResponse_Shell_PostCarriedOnlyWhenDeclared(t *testing.T) {
	spec := dsl.Spec{
		Steps: []dsl.StepEntry{
			{Name: "checkout", Run: "git clone", Shell: []string{"python3", "-c"},
				Post: &dsl.PostStep{Run: "rm -rf ws", Shell: []string{"sh", "-c"}}},
			{Name: "build", Run: "make", Shell: []string{"python3", "-c"},
				Post: &dsl.PostStep{Run: "rm -rf build"}},
		},
	}
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	resp, err := buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j"}, Spec: b})
	require.NoError(t, err)

	require.NotNil(t, resp.Stages[0].Step.Post)
	assert.Equal(t, []string{"sh", "-c"}, resp.Stages[0].Step.Post.Shell, "post declares its own shell")

	require.NotNil(t, resp.Stages[1].Step.Post)
	assert.Nil(t, resp.Stages[1].Step.Post.Shell, "post without shell: is nil on the wire (agent inherits)")
}

// TestBuildClaimResponse_Shell_UsesComposition_TemplateShellWinsOverCaller
// covers the uses: composition end-to-end: a template that declares its own
// spec.shell has that value stamped onto its inlined steps at expansion
// time, and the caller's own spec.shell (present on the outer job that
// hosted the uses: step) does not override it — the inlined step already
// carries a non-empty Shell by the time claim-level resolution runs.
func TestBuildClaimResponse_Shell_UsesComposition_TemplateShellWinsOverCaller(t *testing.T) {
	tplSpec := dsl.Spec{
		Shell: []string{"python3", "-c"},
		Steps: []dsl.StepEntry{{Name: "build", Run: "print('hi')"}},
	}
	expanded, err := gittemplate.ExpandUsesStep("tpl", nil, tplSpec, nil, "", "")
	require.NoError(t, err)

	spec := dsl.Spec{
		Shell: []string{"bash", "-lc"}, // caller job-level default
		Steps: expanded,
	}
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	resp, err := buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j"}, Spec: b})
	require.NoError(t, err)

	var build *api.ClaimStep
	for _, st := range resp.Stages {
		if st.Step != nil && st.Step.Name == "tpl__build" {
			build = st.Step
		}
	}
	require.NotNil(t, build, "expected inlined step tpl__build")
	assert.Equal(t, []string{"python3", "-c"}, build.Shell, "template spec.shell must win over caller spec.shell")
}

// TestBuildClaimResponse_Shell_UsesComposition_CallerFillsUndeclaredTemplate
// covers the other composition case: a template that declares neither its
// own step-level shell nor a template-level spec.shell leaves its inlined
// step with a nil Shell after expansion, so the caller's spec.shell resolves
// onto the final ClaimStep at claim-build time.
func TestBuildClaimResponse_Shell_UsesComposition_CallerFillsUndeclaredTemplate(t *testing.T) {
	tplSpec := dsl.Spec{
		Steps: []dsl.StepEntry{{Name: "build", Run: "make"}},
	}
	expanded, err := gittemplate.ExpandUsesStep("tpl", nil, tplSpec, nil, "", "")
	require.NoError(t, err)

	spec := dsl.Spec{
		Shell: []string{"bash", "-lc"}, // caller job-level default
		Steps: expanded,
	}
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	resp, err := buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j"}, Spec: b})
	require.NoError(t, err)

	var build *api.ClaimStep
	for _, st := range resp.Stages {
		if st.Step != nil && st.Step.Name == "tpl__build" {
			build = st.Step
		}
	}
	require.NotNil(t, build, "expected inlined step tpl__build")
	assert.Equal(t, []string{"bash", "-lc"}, build.Shell, "undeclared template step must pick up the caller's spec.shell")
}

func TestBuildOneClaimStep_CarriesRetry(t *testing.T) {
	entry := dsl.StepEntry{Name: "flaky", Run: "true", Retry: &dsl.RetrySpec{Attempts: 3, Backoff: "30s"}}
	cs := buildOneClaimStep(0, 0, entry, nil)
	require.NotNil(t, cs.Retry)
	assert.Equal(t, 3, cs.Retry.Attempts)
	assert.Equal(t, "30s", cs.Retry.Backoff)
}

// TestAgentAPI_CreateChildRun_OwnedParent verifies an enrolled agent can create
// a child run for a run it has claimed (the call: step path), authorized purely
// by parent-run ownership.
func TestAgentAPI_CreateChildRun_OwnedParent(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "parent-job", "unified-cd/v1", []byte(`{}`))
	_, _ = pg.UpsertJob(t.Context(), "child-job", "unified-cd/v1", []byte(`{"native":true}`))
	parent, _ := pg.CreateRun(t.Context(), "parent-job", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a1", parent.ID)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	body, _ := json.Marshal(api.TriggerRunRequest{JobName: "child-job"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/runs/"+parent.ID+"/children", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var child api.Run
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &child))
	assert.NotEmpty(t, child.ID)
	assert.Equal(t, "child-job", child.JobName)
	assert.NotEqual(t, parent.ID, child.ID)
	assert.Equal(t, "agent:a1", child.TriggeredBy, "child run must be attributed to the spawning agent")
}

// TestAgentAPI_CreateChildRun_NotOwnedParent verifies an agent cannot spawn a
// child for a run claimed by a DIFFERENT agent (403, no run created).
func TestAgentAPI_CreateChildRun_NotOwnedParent(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	_, _ = pg.UpsertJob(t.Context(), "child-job", "unified-cd/v1", []byte(`{"native":true}`))
	parent, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a2", parent.ID) // owned by a2, not a1
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	body, _ := json.Marshal(api.TriggerRunRequest{JobName: "child-job"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/runs/"+parent.ID+"/children", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestAgentAPI_CreateChildRun_TerminalParent verifies that an agent cannot
// spawn a child under a parent run that has already reached a terminal state
// (e.g. the reaper Failed it before the call: step's spawn request arrived).
// agentRunGuard is invoked with rejectTerminal=true here, so a terminal
// parent yields the runWriteTerminal verdict — respondRunWriteVerdict
// replies 200 with an alreadyFinalized body (mirroring
// TestAgentAPI_FinishRun_AlreadyTerminal) rather than creating a run, so this
// asserts no child run was created rather than a 4xx status.
func TestAgentAPI_CreateChildRun_TerminalParent(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	_, _ = pg.UpsertJob(t.Context(), "child-job", "unified-cd/v1", []byte(`{"native":true}`))
	parent, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a1", parent.ID)
	// Simulate the reaper finalizing the parent before the call: step's spawn
	// request arrives.
	require.NoError(t, pg.MarkRunFinished(t.Context(), parent.ID, api.RunFailed))
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	body, _ := json.Marshal(api.TriggerRunRequest{JobName: "child-job"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/runs/"+parent.ID+"/children", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["alreadyFinalized"], "terminal parent must short-circuit to the alreadyFinalized body, not create a run")

	// Direct proof no child run was created: no run for child-job exists.
	childRuns, err := pg.ListRunsByJob(t.Context(), "child-job", 50)
	require.NoError(t, err)
	assert.Empty(t, childRuns, "no child run must be created when the parent is already terminal")
}
