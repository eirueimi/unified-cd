package k8sagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// orchestrateHarness stands up a mock controller, records step statuses by
// name and the final run status, and runs orchestrate with a fake stepExec.
type orchestrateHarness struct {
	statuses map[string]string
	runState string // current run status served by GetRun
	final    string // status passed to FinishRun
	// approvalDecision is the terminal status GetApproval serves after the
	// first (Pending) poll, e.g. "Approved" or "Rejected". Empty disables the
	// approval endpoints' terminal transition (stays Pending).
	approvalDecision string
}

// fakeStep describes what a fake step exec should return.
type fakeStep struct {
	exit   int
	stdout string
}

func runOrchestrate(t *testing.T, c api.ClaimResponse, fakes map[string]fakeStep) (map[string]string, string) {
	t.Helper()
	return runOrchestrateWithApproval(t, c, fakes, "")
}

// runOrchestrateWithApproval is runOrchestrate with a controllable approval
// decision. The approval endpoints serve Pending on the first GetApproval poll
// and approvalDecision thereafter.
func runOrchestrateWithApproval(t *testing.T, c api.ClaimResponse, fakes map[string]fakeStep, approvalDecision string) (map[string]string, string) {
	t.Helper()

	// Speed up approval polling so the Pending->decided transition is fast.
	prevPoll := approvalPollInterval
	approvalPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { approvalPollInterval = prevPoll })

	h := &orchestrateHarness{statuses: map[string]string{}, runState: "Running", approvalDecision: approvalDecision}
	var mu sync.Mutex
	var approvalPolls atomic.Int64

	mux := http.NewServeMux()

	// ReportStep: POST /api/v1/agents/{agentID}/steps
	mux.HandleFunc("POST /api/v1/agents/{id}/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		_ = orchestrateDecodeJSON(r, &req)
		mu.Lock()
		if req.StepName != "" {
			h.statuses[req.StepName] = req.Status
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	// AppendLog: POST /api/v1/agents/{agentID}/logs
	mux.HandleFunc("POST /api/v1/agents/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// SetStepOutputs: POST /api/v1/agents/{agentID}/runs/{runId}/steps/{idx}/outputs
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/steps/{idx}/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// SetRunOutputs: POST /api/v1/agents/{agentID}/runs/{runId}/outputs
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// CreateApproval: POST /api/v1/agents/{id}/runs/{runId}/approvals
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/approvals", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	// GetApproval: GET /api/v1/agents/{id}/runs/{runId}/approvals/{idx}
	// Serves Pending on the first poll, then approvalDecision thereafter.
	mux.HandleFunc("GET /api/v1/agents/{id}/runs/{runId}/approvals/{idx}", func(w http.ResponseWriter, r *http.Request) {
		n := approvalPolls.Add(1)
		status := "Pending"
		if n > 1 && h.approvalDecision != "" {
			status = h.approvalDecision
		}
		orchestrateWriteJSON(w, api.RunApproval{RunID: c.RunID, Status: status})
	})

	// GetRun: GET /api/v1/runs/{id}
	mux.HandleFunc("GET /api/v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		st := h.runState
		mu.Unlock()
		orchestrateWriteJSON(w, api.Run{ID: c.RunID, Status: api.RunStatus(st)})
	})

	// FinishRun: POST /api/v1/agents/{agentID}/runs/{runId}/finish
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/finish", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Status string `json:"status"`
		}
		_ = orchestrateDecodeJSON(r, &req)
		mu.Lock()
		h.final = req.Status
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := agentlib.NewClient(srv.URL, "tok")
	a := &K8sAgent{cfg: Config{AgentID: "k8s-1"}, client: client}

	stepExec := func(_ context.Context, step api.ClaimStep, _ string) (int, string, error) {
		f, ok := fakes[step.Name]
		if !ok {
			return 0, "", nil
		}
		return f.exit, f.stdout, nil
	}
	noopArtifactExec := func(_ context.Context, _, _ string) (int, error) { return 0, nil }
	a.orchestrate(context.Background(), c, stepExec, noopArtifactExec, "/workspace")

	mu.Lock()
	defer mu.Unlock()
	return h.statuses, h.final
}

func TestOrchestrate_FailureSkipsRest(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "x"}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "after", Run: "y"}},
	}}
	statuses, final := runOrchestrate(t, c, map[string]fakeStep{"boom": {exit: 1}})
	assert.Equal(t, "Failed", statuses["boom"])
	assert.Equal(t, "Skipped", statuses["after"], "no-if step auto-skips after a failure")
	assert.Equal(t, "Failed", final)
}

func TestOrchestrate_AlwaysStepRunsAfterFailure(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "x"}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "cleanup", If: "always()", Run: "y"}},
	}}
	statuses, final := runOrchestrate(t, c, map[string]fakeStep{"boom": {exit: 1}})
	assert.Equal(t, "Succeeded", statuses["cleanup"])
	assert.Equal(t, "Failed", final)
}

func TestOrchestrate_ContinueOnErrorDoesNotFailRun(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "flaky", Run: "x", ContinueOnError: true}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "after", Run: "y"}},
	}}
	statuses, final := runOrchestrate(t, c, map[string]fakeStep{"flaky": {exit: 1}})
	assert.Equal(t, "Failed", statuses["flaky"])
	assert.Equal(t, "Succeeded", statuses["after"], "continueOnError failure does not block later steps")
	assert.Equal(t, "Succeeded", final)
}

func TestOrchestrate_FinallyRunsOnFailure(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "x"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 1, StageIndex: 0, Name: "notify", Run: "y"}},
			{Step: &api.ClaimStep{Index: 2, StageIndex: 1, Name: "rollback", If: "failure()", Run: "z"}},
			{Step: &api.ClaimStep{Index: 3, StageIndex: 2, Name: "only-ok", If: "success()", Run: "w"}},
		}}
	statuses, final := runOrchestrate(t, c, map[string]fakeStep{"boom": {exit: 1}})
	assert.Equal(t, "Succeeded", statuses["notify"], "no-if finally step always runs")
	assert.Equal(t, "Succeeded", statuses["rollback"], "failure() runs on failure")
	assert.Equal(t, "Skipped", statuses["only-ok"], "success() skips on failure")
	assert.Equal(t, "Failed", final)
}

func TestOrchestrate_FinallyStepFailureMarksRunFailed(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "ok", Run: "x"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 1, StageIndex: 0, Name: "cleanup-boom", Run: "y"}},
			{Step: &api.ClaimStep{Index: 2, StageIndex: 1, Name: "cleanup-after", Run: "z"}},
		}}
	statuses, final := runOrchestrate(t, c, map[string]fakeStep{"cleanup-boom": {exit: 1}})
	assert.Equal(t, "Failed", statuses["cleanup-boom"])
	assert.Equal(t, "Succeeded", statuses["cleanup-after"], "all finally steps run to completion")
	assert.Equal(t, "Failed", final)
}

func TestOrchestrate_ForeachSkippedAfterFailure(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "x"}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "fan", Run: "echo {{ .Foreach.item }}",
			Foreach: &api.ClaimForeachDef{Key: "item", Source: api.ClaimForeachSource{Literal: []string{"a", "b"}}}}},
	}}
	statuses, final := runOrchestrate(t, c, map[string]fakeStep{"boom": {exit: 1}})
	assert.Equal(t, "Failed", statuses["boom"])
	assert.Equal(t, "Skipped", statuses["fan"], "foreach variants auto-skip after a failure")
	assert.Equal(t, "Failed", final)
}

func TestOrchestrate_ApprovalApprovedRunsLaterStep(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "gate",
			Approval: &api.ClaimApproval{Message: "ok?", TimeoutMinutes: 1}}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "after", Run: "y"}},
	}}
	statuses, final := runOrchestrateWithApproval(t, c, nil, "Approved")
	assert.Equal(t, "Succeeded", statuses["gate"], "approved gate reports Succeeded")
	assert.Equal(t, "Succeeded", statuses["after"], "later step runs after an approved gate")
	assert.Equal(t, "Succeeded", final)
}

func TestOrchestrate_ApprovalRejectedSkipsLaterStep(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "gate",
			Approval: &api.ClaimApproval{Message: "ok?", TimeoutMinutes: 1}}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "after", Run: "y"}},
	}}
	statuses, final := runOrchestrateWithApproval(t, c, nil, "Rejected")
	assert.Equal(t, "Failed", statuses["gate"], "rejected gate reports Failed")
	assert.Equal(t, "Skipped", statuses["after"], "no-if step auto-skips after a rejected gate")
	assert.Equal(t, "Failed", final)
}

// artifactCall records a single call to the fake artifactExec.
type artifactCall struct {
	container string
	script    string
}

// runOrchestrateArtifact is like runOrchestrate but injects a fake
// artifactExec that records (container, script) calls and returns exitCode
// for every call. It returns the recorded calls, per-step statuses, and the
// final run status.
func runOrchestrateArtifact(t *testing.T, c api.ClaimResponse, exitCode int) ([]artifactCall, map[string]string, string) {
	t.Helper()

	h := &orchestrateHarness{statuses: map[string]string{}, runState: "Running"}
	var mu sync.Mutex
	var recorded []artifactCall

	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/agents/{id}/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		_ = orchestrateDecodeJSON(r, &req)
		mu.Lock()
		if req.StepName != "" {
			h.statuses[req.StepName] = req.Status
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/steps/{idx}/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		st := h.runState
		mu.Unlock()
		orchestrateWriteJSON(w, api.Run{ID: c.RunID, Status: api.RunStatus(st)})
	})
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/finish", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Status string `json:"status"`
		}
		_ = orchestrateDecodeJSON(r, &req)
		mu.Lock()
		h.final = req.Status
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := agentlib.NewClient(srv.URL, "tok")
	a := &K8sAgent{cfg: Config{AgentID: "k8s-1"}, client: client}

	fakeStepExec := func(_ context.Context, step api.ClaimStep, _ string) (int, string, error) {
		return 0, "", nil
	}
	fakeArtifactExec := func(_ context.Context, container, script string) (int, error) {
		mu.Lock()
		recorded = append(recorded, artifactCall{container: container, script: script})
		mu.Unlock()
		return exitCode, nil
	}

	a.orchestrate(context.Background(), c, fakeStepExec, fakeArtifactExec, "/workspace")

	mu.Lock()
	defer mu.Unlock()
	return recorded, h.statuses, h.final
}

func TestOrchestrate_UploadArtifactDispatchesToSidecar(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "up",
			UploadArtifact: &api.UploadArtifactStep{Name: "app", Path: "bin/app"}}},
	}}
	rec, statuses, final := runOrchestrateArtifact(t, c, 0 /*exit*/)
	require.Len(t, rec, 1)
	assert.Equal(t, artifactSidecarName, rec[0].container)
	assert.Contains(t, rec[0].script, "tar cf -")
	assert.Contains(t, rec[0].script, "zstd")
	assert.Contains(t, rec[0].script, "-X PUT")
	assert.Contains(t, rec[0].script, "/api/v1/runs/r1/artifacts/app")
	assert.Equal(t, "Succeeded", statuses["up"])
	assert.Equal(t, "Succeeded", final)
}

func TestOrchestrate_DownloadArtifactDispatchesToSidecar(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "dl",
			DownloadArtifact: &api.DownloadArtifactStep{Name: "app", DestDir: "out"}}},
	}}
	rec, statuses, _ := runOrchestrateArtifact(t, c, 0)
	require.Len(t, rec, 1)
	assert.Equal(t, artifactSidecarName, rec[0].container)
	assert.Contains(t, rec[0].script, "curl")
	assert.Contains(t, rec[0].script, "zstd -d")
	assert.Contains(t, rec[0].script, "tar xf -")
	assert.Contains(t, rec[0].script, "/api/v1/runs/r1/artifacts/app")
	assert.Equal(t, "Succeeded", statuses["dl"])
}

func TestOrchestrate_ArtifactExecFailureFailsRun(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "up",
			UploadArtifact: &api.UploadArtifactStep{Name: "app", Path: "bin/app"}}},
	}}
	_, statuses, final := runOrchestrateArtifact(t, c, 1 /*non-zero exit*/)
	assert.Equal(t, "Failed", statuses["up"])
	assert.Equal(t, "Failed", final)
}

// orchestrateDecodeJSON decodes an HTTP request body as JSON into v.
func orchestrateDecodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// orchestrateWriteJSON encodes v as JSON and writes it to the response.
func orchestrateWriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
