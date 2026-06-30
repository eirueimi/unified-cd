package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
)

// runApprovalJob stands up a mock controller that serves the agent approval
// endpoints. The GET approval handler returns "Pending" on the first poll and
// the supplied decided status ("Approved"/"Rejected") on every subsequent poll,
// exercising the polling loop. Steps run through executeRun with a short poll
// interval injected via the dispatch. Returns the last-reported status per step
// name plus the final FinishRun status.
func runApprovalJob(t *testing.T, stages []api.ClaimStage, decided string) (map[string]string, string) {
	t.Helper()

	// Speed up polling so the Pending->decided transition is exercised quickly.
	prevPoll := approvalPollInterval
	approvalPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { approvalPollInterval = prevPoll })

	const agentID = "approval-agent"
	const runID = "run-approval"

	var mu sync.Mutex
	stepStatuses := map[string]string{}
	finishCh := make(chan string, 1)

	var createCalls atomic.Int32
	var getCalls atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		if req.StepName != "" {
			mu.Lock()
			stepStatuses[req.StepName] = req.Status
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// approval create endpoint (Task 5): POST .../approvals -> 204
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/approvals", func(w http.ResponseWriter, r *http.Request) {
		createCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	// approval get endpoint (Task 5): GET .../approvals/{stepIndex} -> RunApproval.
	// First poll returns Pending, subsequent polls return the decided status.
	mux.HandleFunc("GET /api/v1/agents/"+agentID+"/runs/"+runID+"/approvals/{stepIndex}", func(w http.ResponseWriter, r *http.Request) {
		n := getCalls.Add(1)
		status := "Pending"
		if n > 1 {
			status = decided
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.RunApproval{ //nolint:errcheck
			RunID:     runID,
			StepIndex: 0,
			Status:    status,
		})
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishCh <- body.Status:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{
		ID:     agentID,
		Client: NewClient(srv.URL, "tok"),
	}

	resp := api.ClaimResponse{
		RunID:   runID,
		JobName: "test-approval",
		Stages:  stages,
	}

	a.executeRun(context.Background(), resp, t.TempDir())

	var runStatus string
	select {
	case runStatus = <-finishCh:
	default:
		t.Fatal("FinishRun was not called")
	}

	mu.Lock()
	defer mu.Unlock()
	out := make(map[string]string, len(stepStatuses))
	for k, v := range stepStatuses {
		out[k] = v
	}
	return out, runStatus
}

func TestApproval_ApprovedGateRunsLaterStep(t *testing.T) {
	// Job: run "build", approval "gate" (Approved), run "deploy".
	// Expect: gate -> Succeeded, deploy runs (Succeeded), run -> Succeeded.
	stages := []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "build", Run: "echo build"}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "gate", Approval: &api.ClaimApproval{Message: "ok?"}}},
		{Step: &api.ClaimStep{Index: 2, StageIndex: 2, Name: "deploy", Run: "echo deploy"}},
	}
	statuses, runStatus := runApprovalJob(t, stages, "Approved")
	assert.Equal(t, "Succeeded", statuses["build"])
	assert.Equal(t, "Succeeded", statuses["gate"], "approved gate succeeds")
	assert.Equal(t, "Succeeded", statuses["deploy"], "later step runs after approval")
	assert.Equal(t, "Succeeded", runStatus)
}

func TestApproval_RejectedGateSkipsLaterStep(t *testing.T) {
	// Job: run "build", approval "gate" (Rejected), run "deploy".
	// Expect: gate -> Failed, deploy -> Skipped, run -> Failed.
	stages := []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "build", Run: "echo build"}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "gate", Approval: &api.ClaimApproval{Message: "ok?"}}},
		{Step: &api.ClaimStep{Index: 2, StageIndex: 2, Name: "deploy", Run: "echo deploy"}},
	}
	statuses, runStatus := runApprovalJob(t, stages, "Rejected")
	assert.Equal(t, "Failed", statuses["gate"], "rejected gate fails")
	assert.Equal(t, "Skipped", statuses["deploy"], "later step skipped after rejection")
	assert.Equal(t, "Failed", runStatus)
}
