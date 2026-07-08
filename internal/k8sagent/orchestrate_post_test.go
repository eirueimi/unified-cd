package k8sagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newOrchestrateMux builds the same minimal mock-controller endpoint set used
// throughout the orchestrate_*_test.go files (ReportStep/AppendLog/
// SetStepOutputs/SetRunOutputs/GetRun/FinishRun), recording step statuses and
// the final run status into h. mu guards concurrent access to h and to
// whatever the caller records from its own fakes (postExec, etc.).
func newOrchestrateMux(t *testing.T, runID string, h *orchestrateHarness, mu *sync.Mutex) *http.ServeMux {
	t.Helper()
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
		mu.Lock()
		st := h.runState
		mu.Unlock()
		orchestrateWriteJSON(w, api.Run{ID: runID, Status: api.RunStatus(st)})
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
	return mux
}

// newOrchestrateClient starts an httptest.Server for mux (cleaned up via
// t.Cleanup) and returns an agentlib.Client pointed at it.
func newOrchestrateClient(t *testing.T, mux *http.ServeMux) *agentlib.Client {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return agentlib.NewClient(srv.URL, "tok")
}

// runOrchestratePost is like runOrchestrate but uses the shared fake backend
// so post: hook tests can assert on drain order/routing without a real pod.
// fakes lets each test control per-step exit/behavior.
func runOrchestratePost(t *testing.T, c api.ClaimResponse, fakes map[string]fakeStep) (map[string]string, string, []postExecCall) {
	t.Helper()

	statuses, final, calls := runOrchestrateWithFakes(t, c, fakes)
	return statuses, final, calls
}

// runOrchestrateWithFakes stands up the harness (reusing the same mock
// controller endpoints as runOrchestrate) and returns step statuses, final
// run status, and the recorded postExec calls in invocation order.
func runOrchestrateWithFakes(t *testing.T, c api.ClaimResponse, fakes map[string]fakeStep) (map[string]string, string, []postExecCall) {
	t.Helper()

	h := &orchestrateHarness{statuses: map[string]string{}, runState: "Running"}
	var mu sync.Mutex

	mux := newOrchestrateMux(t, c.RunID, h, &mu)
	srvClient := newOrchestrateClient(t, mux)

	a := &K8sAgent{cfg: Config{AgentID: "k8s-1"}, client: srvClient}

	backend := newFakeK8sBackend()
	backend.Fakes = fakes
	agentlib.RunClaim(context.Background(), srvClient, a.cfg.AgentID, c, backend)

	mu.Lock()
	defer mu.Unlock()
	return h.statuses, h.final, backend.PostCalls
}

// TestOrchestrate_PostHooks_RunInLIFOOrderAfterMainSteps is a regression test
// for TODO #42. post: hook drain (hookStack) is owned by the shared
// orchestration loop (agentlib.RunClaim, internal/agent/orchestrator.go), so
// this exercises the same LIFO-drain code path the host agent uses: two
// Succeeded steps with post hooks must have their hooks drained in LIFO order
// (reverse of success order) after the main steps complete.
func TestOrchestrate_PostHooks_RunInLIFOOrderAfterMainSteps(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "first", Run: "x",
			Post: &api.PostStep{Run: "cleanup-first"}}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "second", Run: "y",
			Post: &api.PostStep{Run: "cleanup-second"}}},
	}}

	statuses, final, postCalls := runOrchestratePost(t, c, nil)

	assert.Equal(t, "Succeeded", statuses["first"])
	assert.Equal(t, "Succeeded", statuses["second"])
	assert.Equal(t, "Succeeded", final)

	require.Len(t, postCalls, 2, "expected both post hooks to run")
	assert.Equal(t, "cleanup-second", postCalls[0].script, "hooks drain LIFO: the later step's hook runs first")
	assert.Equal(t, "cleanup-first", postCalls[1].script)
}

// TestOrchestrate_PostHook_NotQueuedForFailedStep verifies a failed step's
// post: hook is never queued (agentlib.RunClaim only appends to hookStack
// when status == "Succeeded"; this is shared with the host agent, not
// k8s-specific behavior).
func TestOrchestrate_PostHook_NotQueuedForFailedStep(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "x",
			Post: &api.PostStep{Run: "should-not-run"}}},
	}}

	statuses, final, postCalls := runOrchestratePost(t, c, map[string]fakeStep{"boom": {exit: 1}})

	assert.Equal(t, "Failed", statuses["boom"])
	assert.Equal(t, "Failed", final)
	assert.Empty(t, postCalls, "a failed step's post hook must not be queued/run")
}

// TestOrchestrate_PostHookFailure_DoesNotFlipRunStatus verifies a failing
// post hook is only logged, never flips the run status (agentlib.RunClaim's
// hookStack drain only slog.Warn's on error; shared with the host agent).
func TestOrchestrate_PostHookFailure_DoesNotFlipRunStatus(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "ok", Run: "x",
			Post: &api.PostStep{Run: "flaky-cleanup"}}},
	}}

	h := &orchestrateHarness{statuses: map[string]string{}, runState: "Running"}
	var mu sync.Mutex
	mux := newOrchestrateMux(t, c.RunID, h, &mu)
	client := newOrchestrateClient(t, mux)
	a := &K8sAgent{cfg: Config{AgentID: "k8s-1"}, client: client}

	backend := newFakeK8sBackend()
	backend.PostExecFn = func(_ context.Context, _, _, _ string, _ []string) error {
		return assert.AnError
	}

	agentlib.RunClaim(context.Background(), client, a.cfg.AgentID, c, backend)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "Succeeded", h.statuses["ok"])
	assert.Equal(t, "Succeeded", h.final, "a failing post hook must not flip the run status")
}

// TestOrchestrate_ScopedStepPostHook_RoutesToScopePod verifies a scoped
// step's post: hook is routed into its scope pod's "step" container, not the
// default run pod. The routing decision (run the post hook wherever the step
// body ran) is made by agentlib.RunClaim and shared with the host agent; only
// k8sBackend.RunPostHook's concrete pod/container lookup is k8s-specific.
func TestOrchestrate_ScopedStepPostHook_RoutesToScopePod(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "scoped", Run: "x",
			ScopeID: "scope:build", ScopeImage: "golang:1.22",
			Post: &api.PostStep{Run: "scoped-cleanup"}}},
	}}

	h := &orchestrateHarness{statuses: map[string]string{}, runState: "Running"}
	var mu sync.Mutex
	mux := newOrchestrateMux(t, c.RunID, h, &mu)
	client := newOrchestrateClient(t, mux)
	a := &K8sAgent{cfg: Config{AgentID: "k8s-1"}, client: client}

	backend := newFakeK8sBackend()
	agentlib.RunClaim(context.Background(), client, a.cfg.AgentID, c, backend)

	mu.Lock()
	defer mu.Unlock()
	postCalls := backend.PostCalls
	require.Len(t, postCalls, 1)
	assert.Equal(t, "scope-pod-scope:build", postCalls[0].targetPod, "scoped step's post hook must target its scope pod")
	assert.Equal(t, "step", postCalls[0].container)
	assert.Equal(t, "scoped-cleanup", postCalls[0].script)
}

// TestOrchestrate_PostHookEnvExpanded verifies a post hook's env: map reaches
// postExec as "KEY=VALUE" pairs.
func TestOrchestrate_PostHookEnvExpanded(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "ok", Run: "x",
			Post: &api.PostStep{Run: "cleanup", Env: map[string]string{"FOO": "bar"}}}},
	}}

	_, _, postCalls := runOrchestratePost(t, c, nil)

	require.Len(t, postCalls, 1)
	assert.Contains(t, postCalls[0].env, "FOO=bar")
}

// TestOrchestrate_NamedContainerStepPostHook_RoutesToSameContainer is a
// regression test for the post-refactor bug where a non-scoped
// container: step's post: hook lost its container routing: the shared
// orchestrator (agentlib.RunClaim) drained every post hook with an empty
// container string, so a hook that must run in the same named container the
// step body ran in was instead routed to the default pod's default
// container. The fix threads step.Container through postHookEntry at
// queue time (internal/agent/orchestrator.go) so it reaches
// k8sBackend.RunPostHook at drain time.
func TestOrchestrate_NamedContainerStepPostHook_RoutesToSameContainer(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "build", Run: "x",
			Container: "build",
			Post:      &api.PostStep{Run: "cleanup-build"}}},
	}}

	_, _, postCalls := runOrchestratePost(t, c, nil)

	require.Len(t, postCalls, 1)
	assert.Equal(t, "", postCalls[0].targetPod, "non-scoped hook stays on the default pod")
	assert.Equal(t, "build", postCalls[0].container, "post hook must run in the same named container the step body ran in, not the default container")
	assert.Equal(t, "cleanup-build", postCalls[0].script)
}
