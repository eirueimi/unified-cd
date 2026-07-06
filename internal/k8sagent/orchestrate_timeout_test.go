package k8sagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runOrchestrateTimeout is like runOrchestrate but takes a custom StepExecFn
// so a test can supply one that blocks until its ctx is cancelled (simulating
// a slow step for timeout tests without a real sleep).
func runOrchestrateTimeout(t *testing.T, c api.ClaimResponse, stepExecFn func(ctx context.Context, step api.ClaimStep, script string) (int, error)) (map[string]string, string) {
	t.Helper()

	h := &orchestrateHarness{statuses: map[string]string{}, runState: "Running"}
	var mu sync.Mutex

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
		orchestrateWriteJSON(w, api.Run{ID: c.RunID, Status: "Running"})
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

	backend := newFakeK8sBackend()
	backend.StepExecFn = stepExecFn
	a.orchestrate(context.Background(), c, backend, nil)

	mu.Lock()
	defer mu.Unlock()
	return h.statuses, h.final
}

// blockingStepExec returns a StepExecFn that blocks until its ctx is Done,
// then returns the ctx error as an infrastructure error — mirroring the host
// agent's runTreeKilled behavior when a step's context deadline is exceeded
// (internal/agent/runner.go: RunStepCapture returns a non-nil runErr, which
// the caller reports as Failed).
func blockingStepExec() func(ctx context.Context, step api.ClaimStep, script string) (int, error) {
	return func(ctx context.Context, _ api.ClaimStep, _ string) (int, error) {
		<-ctx.Done()
		return -1, ctx.Err()
	}
}

// TestOrchestrate_StepTimeout_ReportsFailed is a RED-first regression test for
// TODO #41: step.TimeoutMinutes must bound the step's exec context so a step
// that never returns still fails fast, mirroring the host agent
// (internal/agent/agent.go:443-447). Before the fix, no per-step WithTimeout
// exists for the default/scope exec paths, so this test hangs (and the
// bounded test context below fails it) against the current orchestrate.
func TestOrchestrate_StepTimeout_ReportsFailed(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "slow", Run: "sleep 100", TimeoutMinutes: 0.001}},
	}}

	done := make(chan struct{})
	var statuses map[string]string
	var final string
	go func() {
		statuses, final = runOrchestrateTimeout(t, c, blockingStepExec())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("orchestrate did not return within 5s; step-level timeout is not bounding the exec context")
	}

	assert.Equal(t, "Failed", statuses["slow"], "a timed-out step must report Failed")
	assert.Equal(t, "Failed", final)
}

// TestOrchestrate_JobTimeout_FailsRun is a RED-first regression test for TODO
// #41: c.TimeoutMinutes must bound the whole run, mirroring the host agent
// (internal/agent/agent.go:264-268). Before the fix, c.TimeoutMinutes is never
// read in internal/k8sagent, so this test hangs against the current
// orchestrate (no job-level deadline is ever applied).
func TestOrchestrate_JobTimeout_FailsRun(t *testing.T) {
	c := api.ClaimResponse{
		RunID:          "r1",
		TimeoutMinutes: 0.001, // ~60ms, job level
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "slow", Run: "sleep 100"}},
		},
	}

	done := make(chan struct{})
	var statuses map[string]string
	var final string
	go func() {
		statuses, final = runOrchestrateTimeout(t, c, blockingStepExec())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("orchestrate did not return within 5s; job-level timeout is not bounding the run context")
	}

	assert.Equal(t, "Failed", statuses["slow"], "a step running past the job timeout must report Failed")
	assert.Equal(t, "Failed", final)
	require.NotEmpty(t, final)
}

// TestOrchestrate_JobTimeout_FinallyStillRuns verifies a job-level timeout
// still lets `finally` steps run (mirroring the host agent's
// context.WithoutCancel(ctx) for finally, internal/agent/agent.go:750),
// instead of the timed-out job context skipping cleanup entirely.
func TestOrchestrate_JobTimeout_FinallyStillRuns(t *testing.T) {
	c := api.ClaimResponse{
		RunID:          "r1",
		TimeoutMinutes: 0.001, // ~60ms, job level
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "slow", Run: "sleep 100"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 1, StageIndex: 0, Name: "cleanup", Run: "echo cleanup"}},
		},
	}

	fakeExec := func(ctx context.Context, step api.ClaimStep, _ string) (int, error) {
		if step.Name == "slow" {
			<-ctx.Done()
			return -1, ctx.Err()
		}
		return 0, nil
	}

	done := make(chan struct{})
	var statuses map[string]string
	var final string
	go func() {
		statuses, final = runOrchestrateTimeout(t, c, fakeExec)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("orchestrate did not return within 5s")
	}

	assert.Equal(t, "Failed", statuses["slow"])
	assert.Equal(t, "Succeeded", statuses["cleanup"], "finally must still run after a job-level timeout")
	assert.Equal(t, "Failed", final)
}
