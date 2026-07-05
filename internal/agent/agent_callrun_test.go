package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runCallStepThroughFakeClient drives a run whose single step is
// `call: { job: <jobName> }` through the mock-HTTP-server harness (mirroring
// agent_finally_test.go / agent_if_test.go). The fake CreateRun returns a
// fixed child run ID; the fake child run reports Succeeded so the call
// completes. It returns the terminal StepReportRequest observed for the call
// step (the one with Status Succeeded or Failed, i.e. not "Running").
func runCallStepThroughFakeClient(t *testing.T, jobName, childRunID string) *api.StepReportRequest {
	t.Helper()

	const agentID = "call-agent"
	const runID = "run-call"

	var mu sync.Mutex
	var terminalReport *api.StepReportRequest

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		if req.Status == "Succeeded" || req.Status == "Failed" {
			mu.Lock()
			reqCopy := req
			terminalReport = &reqCopy
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.Run{ID: childRunID, Status: api.RunSucceeded}) //nolint:errcheck
	})
	mux.HandleFunc("GET /api/v1/runs/"+childRunID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.Run{ID: childRunID, Status: api.RunSucceeded}) //nolint:errcheck
	})
	mux.HandleFunc("GET /api/v1/runs/"+childRunID+"/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.RunOutputs{Outputs: map[string]string{}}) //nolint:errcheck
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{
		ID:     agentID,
		Client: NewClient(srv.URL, "tok"),
	}

	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "test-call",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:      0,
				StageIndex: 0,
				Name:       "call-child",
				Call:       &api.ClaimCallStep{Job: jobName},
			}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	mu.Lock()
	defer mu.Unlock()
	return terminalReport
}

func TestExecuteRun_CallStep_ReportsChildLink(t *testing.T) {
	// Drive a run whose single step is `call: { job: child-job }` through the
	// fake-client harness (mirror agent_finally_test.go). The fake CreateRun
	// returns a known child id; the fake child run reports Succeeded so the
	// call completes. Assert the terminal StepReport for the call step carries
	// ChildRunID == <that id> and CallJobName == "child-job".
	rec := runCallStepThroughFakeClient(t, "child-job", "fixed-child-run-id")
	require.NotNil(t, rec)
	assert.Equal(t, "fixed-child-run-id", rec.ChildRunID)
	assert.Equal(t, "child-job", rec.CallJobName)
}
