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
	"github.com/eirueimi/unified-cd/internal/dsl"
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
	noopSidecarExec := func(_ context.Context, _ string, _ []string) (int, error) { return 0, nil }
	a.orchestrate(context.Background(), c, stepExec, noopSidecarExec, "/workspace")

	mu.Lock()
	defer mu.Unlock()
	return h.statuses, h.final
}

// runOrchestrateVariants is like runOrchestrate but also records the Variant
// reported on each ReportStep call, keyed by the reported step name.
func runOrchestrateVariants(t *testing.T, c api.ClaimResponse, fakes map[string]fakeStep) (map[string]string, map[string]string, string) {
	t.Helper()

	h := &orchestrateHarness{statuses: map[string]string{}, runState: "Running"}
	var mu sync.Mutex
	variants := map[string]string{}

	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/agents/{id}/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		_ = orchestrateDecodeJSON(r, &req)
		mu.Lock()
		if req.StepName != "" {
			h.statuses[req.StepName] = req.Status
			variants[req.StepName] = req.Variant
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

	stepExec := func(_ context.Context, step api.ClaimStep, _ string) (int, string, error) {
		f, ok := fakes[step.Name]
		if !ok {
			return 0, "", nil
		}
		return f.exit, f.stdout, nil
	}
	noopSidecarExec := func(_ context.Context, _ string, _ []string) (int, error) { return 0, nil }
	a.orchestrate(context.Background(), c, stepExec, noopSidecarExec, "/workspace")

	mu.Lock()
	defer mu.Unlock()
	return variants, h.statuses, h.final
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
			Matrix: &api.ClaimMatrixDef{Dimensions: []api.ClaimMatrixDimension{
				{Name: "item", Source: api.ClaimForeachSource{Literal: []string{"a", "b"}}},
			}}}},
	}}
	statuses, final := runOrchestrate(t, c, map[string]fakeStep{"boom": {exit: 1}})
	assert.Equal(t, "Failed", statuses["boom"])
	assert.Equal(t, "Skipped", statuses["fan (a)"], "foreach variants auto-skip after a failure")
	assert.Equal(t, "Skipped", statuses["fan (b)"], "foreach variants auto-skip after a failure")
	assert.Equal(t, "Failed", final)
}

// TestOrchestrate_MatrixTwoDimensionExpansion verifies a 2-dimension matrix
// step expands into one run per combination, with DisplayName "name (v1, v2)"
// and a Variant "v1/v2" reported on each ReportStep call.
func TestOrchestrate_MatrixTwoDimensionExpansion(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "build", Run: "echo build",
			Matrix: &api.ClaimMatrixDef{Dimensions: []api.ClaimMatrixDimension{
				{Name: "target", Source: api.ClaimForeachSource{Literal: []string{"x", "y"}}},
				{Name: "shard", Source: api.ClaimForeachSource{Literal: []string{"1", "2"}}},
			}}}},
	}}

	variants, statuses, final := runOrchestrateVariants(t, c, map[string]fakeStep{"build": {exit: 0}})

	wantNames := []string{"build (x, 1)", "build (x, 2)", "build (y, 1)", "build (y, 2)"}
	for _, name := range wantNames {
		assert.Equal(t, "Succeeded", statuses[name], "expected step %q to run and succeed", name)
	}
	assert.Len(t, statuses, 4, "expected exactly 4 expanded step runs")

	wantVariants := map[string]string{
		"build (x, 1)": "x/1",
		"build (x, 2)": "x/2",
		"build (y, 1)": "y/1",
		"build (y, 2)": "y/2",
	}
	for name, variant := range wantVariants {
		assert.Equal(t, variant, variants[name], "expected Variant %q for step %q", variant, name)
	}

	assert.Equal(t, "Succeeded", final)
}

// TestOrchestrate_MatrixOutputsCaptureMatrixTemplateValue is a regression test
// for review finding C2: the k8s agent's output-template evaluation context
// omitted Matrix/Foreach, so an `outputs:` template referencing
// `{{ .Matrix.x }}` silently evaluated to "" (missingkey=zero) instead of the
// actual per-combination dimension value. It verifies each matrix variant's
// SetStepOutputs call carries the real dimension value, not an empty string.
func TestOrchestrate_MatrixOutputsCaptureMatrixTemplateValue(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{
			Index: 0, StageIndex: 0, Name: "build", Run: "echo build",
			Matrix: &api.ClaimMatrixDef{Dimensions: []api.ClaimMatrixDimension{
				{Name: "os", Source: api.ClaimForeachSource{Literal: []string{"linux", "windows"}}},
				{Name: "arch", Source: api.ClaimForeachSource{Literal: []string{"amd64", "arm64"}}},
			}},
			Outputs: map[string]string{"built": "{{ .Matrix.os }}-{{ .Matrix.arch }}"},
		}},
	}}

	outputsByVariant := runOrchestrateCaptureOutputs(t, c, map[string]fakeStep{"build": {exit: 0}})

	want := map[string]string{
		"linux/amd64":   "linux-amd64",
		"linux/arm64":   "linux-arm64",
		"windows/amd64": "windows-amd64",
		"windows/arm64": "windows-arm64",
	}
	require.Len(t, outputsByVariant, 4, "expected outputs reported for all 4 combinations")
	for variant, want := range want {
		got, ok := outputsByVariant[variant]
		require.True(t, ok, "no outputs reported for variant %q", variant)
		assert.Equal(t, want, got["built"], "outputs.built for variant %q should resolve the real Matrix values, not \"-\"", variant)
	}
}

// runOrchestrateCaptureOutputs is like runOrchestrate but records the outputs
// body posted to SetStepOutputs, keyed by the `variant` query parameter.
func runOrchestrateCaptureOutputs(t *testing.T, c api.ClaimResponse, fakes map[string]fakeStep) map[string]map[string]string {
	t.Helper()

	var mu sync.Mutex
	outputsByVariant := map[string]map[string]string{}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/{id}/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/steps/{idx}/outputs", func(w http.ResponseWriter, r *http.Request) {
		var req api.SetOutputsRequest
		_ = orchestrateDecodeJSON(r, &req)
		variant := r.URL.Query().Get("variant")
		mu.Lock()
		outputsByVariant[variant] = req.Outputs
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		orchestrateWriteJSON(w, api.Run{ID: c.RunID, Status: "Running"})
	})
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/finish", func(w http.ResponseWriter, r *http.Request) {
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
	noopSidecarExec := func(_ context.Context, _ string, _ []string) (int, error) { return 0, nil }
	a.orchestrate(context.Background(), c, stepExec, noopSidecarExec, "/workspace")

	mu.Lock()
	defer mu.Unlock()
	return outputsByVariant
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

// artifactCall records a single call to the fake sidecarExec.
type artifactCall struct {
	container string
	argv      []string
}

// runOrchestrateArtifact is like runOrchestrate but injects a fake
// sidecarExec that records (container, argv) calls and returns exitCode
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
	fakeSidecarExec := func(_ context.Context, container string, argv []string) (int, error) {
		mu.Lock()
		recorded = append(recorded, artifactCall{container: container, argv: argv})
		mu.Unlock()
		return exitCode, nil
	}

	a.orchestrate(context.Background(), c, fakeStepExec, fakeSidecarExec, "/workspace")

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
	assert.Equal(t, []string{"unified-sidecar", "artifact", "upload",
		"--run", "r1", "--name", "app", "--path", "/workspace/bin/app"}, rec[0].argv)
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
	assert.Equal(t, []string{"unified-sidecar", "artifact", "download",
		"--run", "r1", "--name", "app", "--dest", "/workspace/out"}, rec[0].argv)
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

func TestOrchestrate_CacheRestoreAndDeferredSave(t *testing.T) {
	c := api.ClaimResponse{
		RunID:  "r1",
		Params: map[string]string{"v": "1"},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "with-cache",
				Cache: &dsl.CacheStep{Path: "node_modules", Key: "npm-{{.Params.v}}", TTLDays: 7}}},
		},
	}
	rec, statuses, final := runOrchestrateArtifact(t, c, 0 /*exit*/)

	var restoreIdx, saveIdx = -1, -1
	var restoreCall, saveCall *artifactCall
	for i := range rec {
		if len(rec[i].argv) >= 2 && rec[i].argv[0] == "unified-sidecar" && rec[i].argv[1] == "cache" {
			if len(rec[i].argv) >= 3 && rec[i].argv[2] == "restore" {
				require.Equal(t, -1, restoreIdx, "expected exactly one cache restore call, got: %+v", rec)
				restoreIdx = i
				restoreCall = &rec[i]
			}
			if len(rec[i].argv) >= 3 && rec[i].argv[2] == "save" {
				require.Equal(t, -1, saveIdx, "expected exactly one cache save call, got: %+v", rec)
				saveIdx = i
				saveCall = &rec[i]
			}
		}
	}

	require.NotNil(t, restoreCall, "expected a cache restore argv call, got: %+v", rec)
	assert.Equal(t, artifactSidecarName, restoreCall.container)
	assert.Contains(t, restoreCall.argv, "--key")
	assert.Contains(t, restoreCall.argv, "npm-1")
	assert.Contains(t, restoreCall.argv, "--path")
	assert.Contains(t, restoreCall.argv, "/workspace/node_modules")

	require.NotNil(t, saveCall, "expected a cache save argv call after the main stages, got: %+v", rec)
	assert.Equal(t, artifactSidecarName, saveCall.container)
	assert.Contains(t, saveCall.argv, "--key")
	assert.Contains(t, saveCall.argv, "npm-1")

	// The save must be deferred until after the main stages: restore happens
	// at step time, save happens once at the end of the run.
	require.True(t, restoreIdx < saveIdx, "expected cache restore (idx %d) before cache save (idx %d), got: %+v", restoreIdx, saveIdx, rec)

	assert.Equal(t, "Succeeded", statuses["with-cache"])
	assert.Equal(t, "Succeeded", final)
}

func TestOrchestrate_CacheEmptyKeySkips(t *testing.T) {
	c := api.ClaimResponse{
		RunID:  "r1",
		Params: map[string]string{"v": "1"},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "with-cache",
				Cache: &dsl.CacheStep{Path: "node_modules", Key: "{{.Params.missing}}", TTLDays: 7}}},
		},
	}
	rec, statuses, final := runOrchestrateArtifact(t, c, 0 /*exit*/)

	for i := range rec {
		if len(rec[i].argv) >= 2 && rec[i].argv[0] == "unified-sidecar" && rec[i].argv[1] == "cache" {
			t.Fatalf("expected no cache restore/save argv calls for an empty-key template, got: %+v", rec[i])
		}
	}

	assert.Equal(t, "Succeeded", statuses["with-cache"])
	assert.Equal(t, "Succeeded", final)
}

func TestOrchestrate_CachePathTemplateExpanded(t *testing.T) {
	c := api.ClaimResponse{
		RunID:  "r1",
		Params: map[string]string{"dir": "apps/web", "v": "1"},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "with-cache",
				Cache: &dsl.CacheStep{Path: "{{ .Params.dir }}/node_modules", Key: "npm-{{.Params.v}}", TTLDays: 7}}},
		},
	}
	rec, statuses, final := runOrchestrateArtifact(t, c, 0 /*exit*/)

	var restoreCall, saveCall *artifactCall
	for i := range rec {
		if len(rec[i].argv) >= 3 && rec[i].argv[0] == "unified-sidecar" && rec[i].argv[1] == "cache" {
			switch rec[i].argv[2] {
			case "restore":
				restoreCall = &rec[i]
			case "save":
				saveCall = &rec[i]
			}
		}
	}

	require.NotNil(t, restoreCall, "expected a cache restore argv call, got: %+v", rec)
	assert.Contains(t, restoreCall.argv, "/workspace/apps/web/node_modules",
		"restore should target the template-expanded path")
	require.NotNil(t, saveCall, "expected a deferred cache save argv call, got: %+v", rec)
	assert.Contains(t, saveCall.argv, "/workspace/apps/web/node_modules",
		"deferred save should target the template-expanded path")

	assert.Equal(t, "Succeeded", statuses["with-cache"])
	assert.Equal(t, "Succeeded", final)
}

func TestOrchestrate_CacheEmptyPathSkips(t *testing.T) {
	c := api.ClaimResponse{
		RunID:  "r1",
		Params: map[string]string{"v": "1"},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "with-cache",
				Cache: &dsl.CacheStep{Path: "{{ .Params.missing }}", Key: "npm-{{.Params.v}}", TTLDays: 7}}},
		},
	}
	rec, statuses, final := runOrchestrateArtifact(t, c, 0 /*exit*/)

	for i := range rec {
		if len(rec[i].argv) >= 2 && rec[i].argv[0] == "unified-sidecar" && rec[i].argv[1] == "cache" {
			t.Fatalf("expected no cache restore/save argv calls for an empty-path template, got: %+v", rec[i])
		}
	}

	assert.Equal(t, "Succeeded", statuses["with-cache"])
	assert.Equal(t, "Succeeded", final)
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
