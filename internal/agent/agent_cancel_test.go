package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
)

func TestExecuteRun_CancelPropagation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TODO: retryUntilSuccess with context.WithoutCancel keeps retrying after test server closes; Windows socket cleanup slower than Linux")
	}
	finishStatus := make(chan string, 1)
	var getRunCalls atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-cancel/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishStatus <- body["status"]:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-cancel/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/run-cancel", func(w http.ResponseWriter, r *http.Request) {
		n := getRunCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		status := api.RunRunning
		if n >= 2 {
			status = api.RunCancelled
		}
		json.NewEncoder(w).Encode(api.Run{ID: "run-cancel", Status: status}) //nolint:errcheck
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{
		ID:     "a1",
		Client: NewClient(srv.URL, "tok"),
	}

	claim := api.ClaimResponse{
		RunID:   "run-cancel",
		JobName: "test",
		Stages:  []api.ClaimStage{{Step: &api.ClaimStep{Name: "s1", Index: 0, Run: "sleep 30"}}},
	}

	workDir := t.TempDir()
	start := time.Now()
	a.executeRun(context.Background(), claim, workDir)

	elapsed := time.Since(start)
	assert.Less(t, elapsed, 15*time.Second, "executeRun should complete within 15s when cancelled (sleep 30 was interrupted)")

	select {
	case status := <-finishStatus:
		assert.Equal(t, string(api.RunCancelled), status, "FinishRun should be called with Cancelled")
	case <-time.After(3 * time.Second):
		t.Fatal("FinishRun was not called within 3 seconds of executeRun returning")
	}
}

// TestExecuteRun_CancelledStepReportsCancelledStatus verifies that the step
// running when the master cancels the run is reported with terminal status
// "Cancelled" rather than "Failed" or being left un-reported. Before the fix,
// an interrupted step's exec error caused it to be reported "Failed"; on the
// controller/UI side this is the difference between a step correctly showing
// as cancelled vs. incorrectly showing as a failure (or, prior to any final
// report at all, staying stuck on "Running" forever — bug #9c).
func TestExecuteRun_CancelledStepReportsCancelledStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TODO: retryUntilSuccess with context.WithoutCancel keeps retrying after test server closes; Windows socket cleanup slower than Linux")
	}
	var mu sync.Mutex
	stepStatuses := map[string]string{} // stepName -> last reported status
	finishStatus := make(chan string, 1)
	var getRunCalls atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		if req.StepName != "" {
			mu.Lock()
			stepStatuses[req.StepName] = req.Status
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-cancel/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishStatus <- body["status"]:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-cancel/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/run-cancel", func(w http.ResponseWriter, r *http.Request) {
		n := getRunCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		status := api.RunRunning
		if n >= 2 {
			status = api.RunCancelled
		}
		json.NewEncoder(w).Encode(api.Run{ID: "run-cancel", Status: status}) //nolint:errcheck
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{
		ID:     "a1",
		Client: NewClient(srv.URL, "tok"),
	}

	claim := api.ClaimResponse{
		RunID:   "run-cancel",
		JobName: "test",
		Stages:  []api.ClaimStage{{Step: &api.ClaimStep{Name: "long-step", Index: 0, Run: "sleep 30"}}},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	select {
	case status := <-finishStatus:
		assert.Equal(t, string(api.RunCancelled), status)
	case <-time.After(3 * time.Second):
		t.Fatal("FinishRun was not called within 3 seconds of executeRun returning")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "Cancelled", stepStatuses["long-step"],
		"the step running at cancellation time must be reported Cancelled, not Failed or left as Running")
}

// TestExecuteRun_FinallyRunsOnCancel verifies that the finally block runs even when
// the run was cancelled by the master, that failure() is false in finally (cancel is
// not a failure), and that the overall status stays Cancelled when no finally step fails.
func TestExecuteRun_FinallyRunsOnCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TODO: retryUntilSuccess with context.WithoutCancel keeps retrying after test server closes; Windows socket cleanup slower than Linux")
	}
	finishStatus := make(chan string, 1)
	var getRunCalls atomic.Int32

	var mu sync.Mutex
	stepStatuses := map[string]string{}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		if req.StepName != "" {
			mu.Lock()
			stepStatuses[req.StepName] = req.Status
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-cancel/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishStatus <- body["status"]:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-cancel/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-cancel/steps/1/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/run-cancel", func(w http.ResponseWriter, r *http.Request) {
		n := getRunCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		status := api.RunRunning
		if n >= 2 {
			status = api.RunCancelled
		}
		json.NewEncoder(w).Encode(api.Run{ID: "run-cancel", Status: status}) //nolint:errcheck
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{
		ID:     "a1",
		Client: NewClient(srv.URL, "tok"),
	}

	claim := api.ClaimResponse{
		RunID:   "run-cancel",
		JobName: "test",
		Stages:  []api.ClaimStage{{Step: &api.ClaimStep{Name: "s1", Index: 0, Run: "sleep 30"}}},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{Name: "cleanup", Index: 1, StageIndex: 0, Run: "echo cleanup"}},
			{Step: &api.ClaimStep{Name: "on-fail", Index: 2, StageIndex: 1, If: "failure()", Run: "echo nope"}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	select {
	case status := <-finishStatus:
		assert.Equal(t, string(api.RunCancelled), status, "cancelled run with no failing finally step stays Cancelled")
	case <-time.After(3 * time.Second):
		t.Fatal("FinishRun was not called within 3 seconds of executeRun returning")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "Succeeded", stepStatuses["cleanup"], "no-if finally step runs even on cancel")
	assert.Equal(t, "Skipped", stepStatuses["on-fail"], "failure() is false on cancel, so the step is skipped")
}
