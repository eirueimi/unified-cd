package k8sagent

import (
	"context"
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

// TestOrchestrate_ReportRetriesUntilSuccess is a regression test for the k8s
// agent's report calls being single-shot: today a transient 500 from the
// controller on a step's terminal ReportStep or on FinishRun is silently
// dropped (the `_ = a.client.ReportStep(...)` / `_ = a.client.FinishRun(...)`
// discard the error), so the controller never learns the step/run finished.
// It stands up a fake controller that rejects the first 2 ReportStep calls
// and the first FinishRun call with 500, then asserts the terminal step
// report and FinishRun both eventually land once orchestrate retries them.
func TestOrchestrate_ReportRetriesUntilSuccess(t *testing.T) {
	// Speed up the retry backoff so this test doesn't block on real 1s/2s/4s
	// sleeps between attempts.
	prevInitial, prevMax := agentlib.RetryInitialWait, agentlib.RetryMaxWait
	agentlib.RetryInitialWait, agentlib.RetryMaxWait = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { agentlib.RetryInitialWait, agentlib.RetryMaxWait = prevInitial, prevMax })

	var stepReportCalls atomic.Int32
	var finishCalls atomic.Int32

	var mu sync.Mutex
	acceptedStatuses := map[string]string{}
	var acceptedFinish string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/{id}/steps", func(w http.ResponseWriter, r *http.Request) {
		n := stepReportCalls.Add(1)
		var req api.StepReportRequest
		_ = orchestrateDecodeJSON(r, &req)
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mu.Lock()
		if req.StepName != "" {
			acceptedStatuses[req.StepName] = req.Status
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
		orchestrateWriteJSON(w, api.Run{ID: "r1", Status: "Running"})
	})
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/finish", func(w http.ResponseWriter, r *http.Request) {
		n := finishCalls.Add(1)
		if n <= 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var req struct {
			Status string `json:"status"`
		}
		_ = orchestrateDecodeJSON(r, &req)
		mu.Lock()
		acceptedFinish = req.Status
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := agentlib.NewClient(srv.URL, "tok")
	a := &K8sAgent{cfg: Config{AgentID: "k8s-1"}, client: client}

	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "echo", Run: "echo ok"}},
	}}

	stepExec := func(_ context.Context, step api.ClaimStep, _ string) (int, string, error) {
		return 0, "", nil
	}
	noopSidecarExec := func(_ context.Context, _, _ string, _ []string) (int, error) { return 0, nil }
	noopPostExec := func(_ context.Context, _, _, _ string, _ []string) error { return nil }
	noopEnsureScopePod := func(_ context.Context, _ api.ClaimStep) (string, error) { return "", nil }

	done := make(chan struct{})
	go func() {
		a.orchestrate(context.Background(), c, stepExec, noopSidecarExec, noopPostExec, "/workspace", noopEnsureScopePod, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("orchestrate did not return within 10s; retries may not be happening")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, acceptedStatuses, "echo", "the step's terminal report should eventually be accepted despite transient 500s")
	assert.Equal(t, "Succeeded", acceptedStatuses["echo"])
	assert.Equal(t, "Succeeded", acceptedFinish, "FinishRun should eventually be accepted despite a transient 500")
	assert.GreaterOrEqual(t, stepReportCalls.Load(), int32(3), "expected at least 3 ReportStep attempts (2 rejected + 1 accepted)")
	assert.GreaterOrEqual(t, finishCalls.Load(), int32(2), "expected at least 2 FinishRun attempts (1 rejected + 1 accepted)")
}
