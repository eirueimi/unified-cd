package k8sagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
)

// orchestrateHarness stands up a mock controller, records step statuses by
// name and the final run status, and runs orchestrate with a fake stepExec.
type orchestrateHarness struct {
	statuses map[string]string
	runState string // current run status served by GetRun
	final    string // status passed to FinishRun
}

// fakeStep describes what a fake step exec should return.
type fakeStep struct {
	exit   int
	stdout string
}

func runOrchestrate(t *testing.T, c api.ClaimResponse, fakes map[string]fakeStep) (map[string]string, string) {
	t.Helper()
	h := &orchestrateHarness{statuses: map[string]string{}, runState: "Running"}
	var mu sync.Mutex

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
	a.orchestrate(context.Background(), c, stepExec)

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

func TestOrchestrate_NoIfStepSkippedAfterFailure(t *testing.T) {
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

// orchestrateDecodeJSON decodes an HTTP request body as JSON into v.
func orchestrateDecodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// orchestrateWriteJSON encodes v as JSON and writes it to the response.
func orchestrateWriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
