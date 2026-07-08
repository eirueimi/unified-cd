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

// TestExecuteRun_IfFalse_StepSkipped: a step with if: false should report
// "Skipped" and the overall Run should finish as Succeeded.
func TestExecuteRun_IfFalse_StepSkipped(t *testing.T) {
	var mu sync.Mutex
	stepReports := map[int][]string{} // stepIndex → []status

	finishStatus := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		mu.Lock()
		stepReports[req.StepIndex] = append(stepReports[req.StepIndex], req.Status)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-if/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishStatus <- body["status"]:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-if/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/run-if", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.Run{ID: "run-if", Status: api.RunRunning}) //nolint:errcheck
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{ID: "a1", Client: NewClient(srv.URL, "tok")}
	claim := api.ClaimResponse{
		Native:  true,
		RunID:   "run-if",
		JobName: "test",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Name: "skip-me", Index: 0, If: "false", Run: "echo should-not-run"}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	mu.Lock()
	reports := stepReports[0]
	mu.Unlock()

	assert.Equal(t, []string{"Skipped"}, reports, "step 0 should only report Skipped")

	select {
	case s := <-finishStatus:
		assert.Equal(t, "Succeeded", s, "run should finish as Succeeded")
	default:
		t.Fatal("FinishRun not called")
	}
}

// TestExecuteRun_IfFalse_DownstreamStillRuns: a downstream step that needs a
// skipped (if: false) step should still run.
func TestExecuteRun_IfFalse_DownstreamStillRuns(t *testing.T) {
	var mu sync.Mutex
	stepReports := map[int][]string{}

	finishStatus := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		mu.Lock()
		stepReports[req.StepIndex] = append(stepReports[req.StepIndex], req.Status)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-downstream/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishStatus <- body["status"]:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-downstream/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-downstream/steps/2/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/run-downstream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.Run{ID: "run-downstream", Status: api.RunRunning}) //nolint:errcheck
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{ID: "a1", Client: NewClient(srv.URL, "tok")}
	claim := api.ClaimResponse{
		Native:  true,
		RunID:   "run-downstream",
		JobName: "test",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Name: "a", Index: 0, Run: "echo a"}},
			{Step: &api.ClaimStep{Name: "b", Index: 1, If: "false", Run: "echo b"}},
			{Step: &api.ClaimStep{Name: "c", Index: 2, Run: "echo c"}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	mu.Lock()
	reportsA := stepReports[0]
	reportsB := stepReports[1]
	reportsC := stepReports[2]
	mu.Unlock()

	assert.Contains(t, reportsA, "Succeeded", "step a should succeed")
	assert.Equal(t, []string{"Skipped"}, reportsB, "step b should be skipped")
	assert.Contains(t, reportsC, "Succeeded", "step c should succeed (downstream of skipped step)")

	select {
	case s := <-finishStatus:
		assert.Equal(t, "Succeeded", s)
	default:
		t.Fatal("FinishRun not called")
	}
}

// TestExecuteRun_IfTrue_StepRuns: a step with if: true should execute normally.
func TestExecuteRun_IfTrue_StepRuns(t *testing.T) {
	var mu sync.Mutex
	stepReports := map[int][]string{}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		mu.Lock()
		stepReports[req.StepIndex] = append(stepReports[req.StepIndex], req.Status)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-iftrue/finish", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-iftrue/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/run-iftrue", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.Run{ID: "run-iftrue", Status: api.RunRunning}) //nolint:errcheck
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{ID: "a1", Client: NewClient(srv.URL, "tok")}
	claim := api.ClaimResponse{
		Native:  true,
		RunID:   "run-iftrue",
		JobName: "test",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Name: "run-me", Index: 0, If: "true", Run: "echo hello"}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	mu.Lock()
	reports := stepReports[0]
	mu.Unlock()

	assert.Contains(t, reports, "Running", "step with if:true should report Running")
	assert.Contains(t, reports, "Succeeded", "step with if:true should report Succeeded")
	assert.NotContains(t, reports, "Skipped", "step with if:true should not be skipped")
}
