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

// postExecCall records a single call to a fake postExec.
type postExecCall struct {
	targetPod, container, script string
	env                          []string
}

// runOrchestratePost is like runOrchestrate but injects a fake postExec that
// records every call, so post: hook tests can assert on drain order/routing
// without a real pod. stepExec lets each test control per-step exit/behavior.
func runOrchestratePost(t *testing.T, c api.ClaimResponse, stepExec podStepExec) (map[string]string, string, []postExecCall) {
	t.Helper()

	statuses, final, calls := runOrchestrateWithFakes(t, c, stepExec)
	return statuses, final, calls
}

// runOrchestrateWithFakes stands up the harness (reusing the same mock
// controller endpoints as runOrchestrate) and returns step statuses, final
// run status, and the recorded postExec calls in invocation order.
func runOrchestrateWithFakes(t *testing.T, c api.ClaimResponse, stepExec podStepExec) (map[string]string, string, []postExecCall) {
	t.Helper()

	h := &orchestrateHarness{statuses: map[string]string{}, runState: "Running"}
	var mu sync.Mutex
	var postCalls []postExecCall

	mux := newOrchestrateMux(t, c.RunID, h, &mu)
	srvClient := newOrchestrateClient(t, mux)

	a := &K8sAgent{cfg: Config{AgentID: "k8s-1"}, client: srvClient}

	noopSidecarExec := func(_ context.Context, _, _ string, _ []string) (int, error) { return 0, nil }
	noopEnsureScopePod := func(_ context.Context, _ api.ClaimStep) (string, error) { return "", nil }
	fakePostExec := func(_ context.Context, targetPod, container, script string, env []string) error {
		mu.Lock()
		postCalls = append(postCalls, postExecCall{targetPod: targetPod, container: container, script: script, env: env})
		mu.Unlock()
		return nil
	}

	a.orchestrate(context.Background(), c, stepExec, noopSidecarExec, fakePostExec, "/workspace", noopEnsureScopePod)

	mu.Lock()
	defer mu.Unlock()
	return h.statuses, h.final, postCalls
}

// TestOrchestrate_PostHooks_RunInLIFOOrderAfterMainSteps is a RED-first
// regression test for TODO #42: step.Post is never referenced in
// internal/k8sagent, so post: hooks never run. Mirrors the host agent
// (internal/agent/agent.go:664-674, 707-734): two Succeeded steps with post
// hooks must have their hooks drained in LIFO order (reverse of success
// order) after the main steps complete.
func TestOrchestrate_PostHooks_RunInLIFOOrderAfterMainSteps(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "first", Run: "x",
			Post: &api.PostStep{Run: "cleanup-first"}}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "second", Run: "y",
			Post: &api.PostStep{Run: "cleanup-second"}}},
	}}

	stepExec := func(_ context.Context, step api.ClaimStep, _ string) (int, string, error) {
		return 0, "", nil
	}

	statuses, final, postCalls := runOrchestratePost(t, c, stepExec)

	assert.Equal(t, "Succeeded", statuses["first"])
	assert.Equal(t, "Succeeded", statuses["second"])
	assert.Equal(t, "Succeeded", final)

	require.Len(t, postCalls, 2, "expected both post hooks to run")
	assert.Equal(t, "cleanup-second", postCalls[0].script, "hooks drain LIFO: the later step's hook runs first")
	assert.Equal(t, "cleanup-first", postCalls[1].script)
}

// TestOrchestrate_PostHook_NotQueuedForFailedStep verifies a failed step's
// post: hook is never queued (mirrors the host: hookStack is only appended
// when status == "Succeeded").
func TestOrchestrate_PostHook_NotQueuedForFailedStep(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "x",
			Post: &api.PostStep{Run: "should-not-run"}}},
	}}

	stepExec := func(_ context.Context, step api.ClaimStep, _ string) (int, string, error) {
		return 1, "", nil
	}

	statuses, final, postCalls := runOrchestratePost(t, c, stepExec)

	assert.Equal(t, "Failed", statuses["boom"])
	assert.Equal(t, "Failed", final)
	assert.Empty(t, postCalls, "a failed step's post hook must not be queued/run")
}

// TestOrchestrate_PostHookFailure_DoesNotFlipRunStatus verifies a failing
// post hook is only logged, never flips the run status (mirrors the host:
// hookStack drain only slog.Warn's on error).
func TestOrchestrate_PostHookFailure_DoesNotFlipRunStatus(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "ok", Run: "x",
			Post: &api.PostStep{Run: "flaky-cleanup"}}},
	}}

	stepExec := func(_ context.Context, step api.ClaimStep, _ string) (int, string, error) {
		return 0, "", nil
	}

	h := &orchestrateHarness{statuses: map[string]string{}, runState: "Running"}
	var mu sync.Mutex
	mux := newOrchestrateMux(t, c.RunID, h, &mu)
	client := newOrchestrateClient(t, mux)
	a := &K8sAgent{cfg: Config{AgentID: "k8s-1"}, client: client}

	noopSidecarExec := func(_ context.Context, _, _ string, _ []string) (int, error) { return 0, nil }
	noopEnsureScopePod := func(_ context.Context, _ api.ClaimStep) (string, error) { return "", nil }
	failingPostExec := func(_ context.Context, _, _, _ string, _ []string) error {
		return assert.AnError
	}

	a.orchestrate(context.Background(), c, stepExec, noopSidecarExec, failingPostExec, "/workspace", noopEnsureScopePod)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "Succeeded", h.statuses["ok"])
	assert.Equal(t, "Succeeded", h.final, "a failing post hook must not flip the run status")
}

// TestOrchestrate_ScopedStepPostHook_RoutesToScopePod verifies a scoped
// step's post: hook is routed into its scope pod's "step" container, not the
// default run pod (mirrors the host: a scoped step's post hook runs inside
// the same scope container the step body ran in).
func TestOrchestrate_ScopedStepPostHook_RoutesToScopePod(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "scoped", Run: "x",
			ScopeID: "scope:build", ScopeImage: "golang:1.22",
			Post: &api.PostStep{Run: "scoped-cleanup"}}},
	}}

	stepExec := func(_ context.Context, step api.ClaimStep, _ string) (int, string, error) {
		return 0, "", nil
	}

	h := &orchestrateHarness{statuses: map[string]string{}, runState: "Running"}
	var mu sync.Mutex
	var postCalls []postExecCall
	mux := newOrchestrateMux(t, c.RunID, h, &mu)
	client := newOrchestrateClient(t, mux)
	a := &K8sAgent{cfg: Config{AgentID: "k8s-1"}, client: client}

	noopSidecarExec := func(_ context.Context, _, _ string, _ []string) (int, error) { return 0, nil }
	fakeEnsureScopePod := func(_ context.Context, step api.ClaimStep) (string, error) {
		return "scope-pod-" + step.ScopeID, nil
	}
	fakePostExec := func(_ context.Context, targetPod, container, script string, env []string) error {
		mu.Lock()
		postCalls = append(postCalls, postExecCall{targetPod: targetPod, container: container, script: script, env: env})
		mu.Unlock()
		return nil
	}

	a.orchestrate(context.Background(), c, stepExec, noopSidecarExec, fakePostExec, "/workspace", fakeEnsureScopePod)

	mu.Lock()
	defer mu.Unlock()
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

	stepExec := func(_ context.Context, step api.ClaimStep, _ string) (int, string, error) {
		return 0, "", nil
	}

	_, _, postCalls := runOrchestratePost(t, c, stepExec)

	require.Len(t, postCalls, 1)
	assert.Contains(t, postCalls[0].env, "FOO=bar")
}
