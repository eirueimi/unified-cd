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
)

// cancelHarness stands up a mock controller like orchestrateHarness, but lets a
// per-step stepExec hook drive the run state (to flip it to "Cancelled") and
// observe the execCtx a step is invoked with (to assert it is interrupted when a
// cancellation arrives mid-run).
type cancelHarness struct {
	statuses map[string]string
	runState string
	final    string
}

// runOrchestrateCancel runs orchestrate against a mock controller with a
// controllable run state. The stepFn callback is invoked for each step and
// returns the exit code; it receives the harness (so it can flip runState) and
// the step's execCtx (so it can observe/await interruption).
func runOrchestrateCancel(
	t *testing.T,
	c api.ClaimResponse,
	stepFn func(h *cancelHarness, mu *sync.Mutex, execCtx context.Context, step api.ClaimStep) int,
) (map[string]string, string) {
	t.Helper()

	// Shorten poll intervals so tests are fast.
	prevCancel := cancelPollInterval
	cancelPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { cancelPollInterval = prevCancel })
	prevPoll := approvalPollInterval
	approvalPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { approvalPollInterval = prevPoll })

	h := &cancelHarness{statuses: map[string]string{}, runState: "Running"}
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

	// GetRun: serves the harness's current run state.
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

	stepExec := func(execCtx context.Context, step api.ClaimStep, _ string) (int, string, error) {
		if stepFn == nil {
			return 0, "", nil
		}
		return stepFn(h, &mu, execCtx, step), "", nil
	}
	noopSidecarExec := func(_ context.Context, _, _ string, _ []string) (int, error) { return 0, nil }
	noopPostExec := func(_ context.Context, _, _, _ string, _ []string) error { return nil }
	noopEnsureScopePod := func(_ context.Context, _ api.ClaimStep) (string, error) { return "", nil }
	a.orchestrate(context.Background(), c, stepExec, noopSidecarExec, noopPostExec, "/workspace", noopEnsureScopePod)

	mu.Lock()
	defer mu.Unlock()
	return h.statuses, h.final
}

// TestOrchestrate_CancelMidRun verifies that when the run is cancelled while a
// step is executing, the step's execCtx is interrupted and the final run status
// is Cancelled (not Failed or Succeeded).
func TestOrchestrate_CancelMidRun(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "long", Run: "x"}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "after", Run: "y"}},
	}}

	var interrupted bool
	statuses, final := runOrchestrateCancel(t, c, func(h *cancelHarness, mu *sync.Mutex, execCtx context.Context, step api.ClaimStep) int {
		if step.Name == "long" {
			// Flip the run to Cancelled; the poller should observe it and cancel
			// the execCtx passed to this step.
			mu.Lock()
			h.runState = "Cancelled"
			mu.Unlock()
			select {
			case <-execCtx.Done():
				interrupted = true
			case <-time.After(5 * time.Second):
			}
			return 1 // cancel-induced failure; must be suppressed
		}
		return 0
	})

	assert.True(t, interrupted, "the in-flight step's execCtx must be cancelled when the run is cancelled")
	assert.Equal(t, "Cancelled", final, "final run status is Cancelled")
	// "after" auto-skips because the cancelled run is not success().
	assert.Equal(t, "Skipped", statuses["after"], "later step is skipped once cancelled")
}

// TestOrchestrate_FinallyRunsOnCancel verifies that finally still runs after a
// cancellation and that failure() is false there (a failure() finally step is
// Skipped, a no-if finally step runs).
func TestOrchestrate_FinallyRunsOnCancel(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "long", Run: "x"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 1, StageIndex: 0, Name: "notify", Run: "y"}},
			{Step: &api.ClaimStep{Index: 2, StageIndex: 1, Name: "rollback", If: "failure()", Run: "z"}},
		}}

	statuses, final := runOrchestrateCancel(t, c, func(h *cancelHarness, mu *sync.Mutex, execCtx context.Context, step api.ClaimStep) int {
		if step.Name == "long" {
			mu.Lock()
			h.runState = "Cancelled"
			mu.Unlock()
			select {
			case <-execCtx.Done():
			case <-time.After(5 * time.Second):
			}
			return 1
		}
		return 0
	})

	assert.Equal(t, "Cancelled", final, "final run status is Cancelled")
	assert.Equal(t, "Succeeded", statuses["notify"], "no-if finally step runs on cancel")
	assert.Equal(t, "Skipped", statuses["rollback"], "failure() is false on cancel, so rollback is skipped")
}
