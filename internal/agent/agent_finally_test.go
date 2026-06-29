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
)

// runJobStages stands up a mock controller server, serves a single
// ClaimResponse{Stages, Finally}, runs one claim through executeRun, and
// returns the last-reported status per step name plus the final FinishRun status.
//
// The finally argument is unused in Task 4 (pass nil); Task 5 will exercise it.
func runJobStages(t *testing.T, stages []api.ClaimStage, finally []api.ClaimStage) (map[string]string, string) {
	t.Helper()

	const agentID = "finally-agent"
	const runID = "run-finally"

	var mu sync.Mutex
	stepStatuses := map[string]string{}
	finishCh := make(chan string, 1)

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
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/1/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
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
		JobName: "test-finally",
		Stages:  stages,
		Finally: finally,
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

func TestExecuteRun_StepAfterFailureIsSkipped(t *testing.T) {
	// Job: step "boom" (exit 1) then step "after" (no if).
	// Expect: boom -> Failed, after -> Skipped, run -> Failed.
	steps := []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "exit 1"}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "after", Run: "echo hi"}},
	}
	statuses, runStatus := runJobStages(t, steps, nil)
	assert.Equal(t, "Failed", statuses["boom"])
	assert.Equal(t, "Skipped", statuses["after"])
	assert.Equal(t, "Failed", runStatus)
}

func TestExecuteRun_AlwaysStepRunsAfterFailure(t *testing.T) {
	steps := []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "exit 1"}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "cleanup", If: "always()", Run: "echo bye"}},
	}
	statuses, runStatus := runJobStages(t, steps, nil)
	assert.Equal(t, "Failed", statuses["boom"])
	assert.Equal(t, "Succeeded", statuses["cleanup"])
	assert.Equal(t, "Failed", runStatus)
}
