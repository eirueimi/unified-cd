package k8sagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A `call:` step on the k8s agent must launch a child Run (like the host agent)
// and wait for it, NOT be silently treated as an empty `run` step. Regression
// test for: k8s had no call handling, so a call step fell through to an empty
// exec, the child job never launched, and the parent run finished (and its pod
// was torn down) immediately.
func TestOrchestrate_CallStepLaunchesChildRun(t *testing.T) {
	const childRunID = "child-run-1"

	var mu sync.Mutex
	statuses := map[string]string{}
	childLinks := map[string]string{} // stepName -> ChildRunID from a terminal report
	var createRunCalled atomic.Bool
	var stepExecCalled atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/{id}/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		_ = orchestrateDecodeJSON(r, &req)
		mu.Lock()
		if req.StepName != "" {
			statuses[req.StepName] = req.Status
			if req.ChildRunID != "" {
				childLinks[req.StepName] = req.ChildRunID
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/{id}/logs", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/steps/{idx}/outputs", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/finish", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	// CreateRun: the call launches the child job here.
	mux.HandleFunc("POST /api/v1/runs", func(w http.ResponseWriter, r *http.Request) {
		createRunCalled.Store(true)
		orchestrateWriteJSON(w, api.Run{ID: childRunID, Status: api.RunSucceeded})
	})
	// GetRun: the child reports Succeeded so the poll completes immediately; the
	// parent run (any other id) stays Running.
	mux.HandleFunc("GET /api/v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		st := api.RunStatus("Running")
		if id == childRunID {
			st = api.RunSucceeded
		}
		orchestrateWriteJSON(w, api.Run{ID: id, Status: st})
	})
	mux.HandleFunc("GET /api/v1/runs/{id}/outputs", func(w http.ResponseWriter, r *http.Request) {
		orchestrateWriteJSON(w, api.RunOutputs{Outputs: map[string]string{}})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := agentlib.NewClient(srv.URL, "tok")
	a := &K8sAgent{cfg: Config{AgentID: "k8s-1"}, client: client}

	c := api.ClaimResponse{
		RunID:   "parent-run",
		JobName: "orchestrator",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, StageIndex: 0, Name: "callChild",
				Call: &api.ClaimCallStep{Job: "child-job"},
			}},
		},
	}

	stepExec := func(_ context.Context, step api.ClaimStep, _ string) (int, string, error) {
		if strings.Contains(step.Name, "callChild") {
			stepExecCalled.Store(true)
		}
		return 0, "", nil
	}
	noopSidecarExec := func(_ context.Context, _, _ string, _ []string) (int, error) { return 0, nil }
	noopEnsureScopePod := func(_ context.Context, _ api.ClaimStep) (string, error) { return "", nil }

	a.orchestrate(context.Background(), c, stepExec, noopSidecarExec, "/workspace", noopEnsureScopePod)

	mu.Lock()
	defer mu.Unlock()
	require.True(t, createRunCalled.Load(), "call step must launch the child run via CreateRun")
	assert.False(t, stepExecCalled.Load(), "call step must NOT fall through to an empty run exec")
	assert.Equal(t, "Succeeded", statuses["callChild"], "call step should succeed when the child succeeds")
	assert.Equal(t, childRunID, childLinks["callChild"], "call step's terminal report must carry the child run link")
}
