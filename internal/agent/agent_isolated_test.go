package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolatedHarness is a minimal fake controller for executeRun's isolated
// (!Native) path. Modeled after guardHarness/newGuardServer
// (orchestrator_outputsguard_test.go): it records exactly what the isolated
// path is supposed to produce end-to-end — step exec routing (via the shared
// podFakeRT from claim_pod_test.go), the finish status, and any
// AppendLogBulk system log lines (stepIndex -1) failClaim/failRun ships.
type isolatedHarness struct {
	mu sync.Mutex

	finishStatus string
	finishCalled bool

	// logsByStep captures every AppendLogBulk line, keyed by stepIndex.
	logsByStep map[int][]api.LogAppendRequest
}

func newIsolatedHarness() *isolatedHarness {
	return &isolatedHarness{logsByStep: map[int][]api.LogAppendRequest{}}
}

// newIsolatedServer wires a httptest server covering every controller
// endpoint RunClaim/executeRun touch during an isolated claim: register,
// step reporting, bulk logs (both per-step and the stepIndex==-1 system log
// failRun/warnSkippedOutput use), cancellation polling (GetRun), and finish.
func newIsolatedServer(t *testing.T, agentID string, h *isolatedHarness) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		idx, _ := strconv.Atoi(r.PathValue("idx"))
		var reqs []api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&reqs)
		h.mu.Lock()
		h.logsByStep[idx] = append(h.logsByStep[idx], reqs...)
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/v1/runs/{runId}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Run{ID: r.PathValue("runId"), Status: api.RunRunning})
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.finishStatus = body.Status
		h.finishCalled = true
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestExecuteRun_Isolated_HappyPath drives executeRun end-to-end for an
// isolated (Native:false) claim with a podTemplate: pause + every podTemplate
// container + the injected "job" primary must be created, the default step's
// exec must target the primary ("job") container, the run must finish
// Succeeded, and every created container (pause included) must be removed at
// claim end (CloseScopes -> hostBackend.CloseScopes -> pod.CloseAll).
func TestExecuteRun_Isolated_HappyPath(t *testing.T) {
	const agentID = "isolated-happy-agent"
	const runID = "run-isolated-happy"

	h := newIsolatedHarness()
	srv := newIsolatedServer(t, agentID, h)

	f := &podFakeRT{}
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok"), PauseImage: "pause:img", RunnerImage: "runner:img"}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = f

	claim := api.ClaimResponse{
		Native:      false,
		RunID:       runID,
		JobName:     "test-isolated-happy",
		PodTemplate: mysqlTemplate(),
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, Name: "s1", Run: "echo hi"}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	// pause + mysql sidecar + injected "job" primary.
	require.Len(t, f.created, 3, "pause + podTemplate container + injected job primary")
	assert.Empty(t, f.created[0].NetworkContainer, "pause owns the netns, joins nothing")
	assert.Equal(t, "mysql:8", f.created[1].Image)
	assert.Equal(t, "runner:img", f.created[2].Image, "job primary injected from RunnerImage")

	// The default step's exec must target the primary ("job") container,
	// created 3rd -> handle id "c2" (see podFakeRT.Create/fmtID).
	require.NotEmpty(t, f.execs)
	assert.Equal(t, "c2", f.execs[0].id, "default step execs into the primary job container")
	assert.Contains(t, f.execs[0].script, "echo hi")

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.True(t, h.finishCalled)
	assert.Equal(t, "Succeeded", h.finishStatus)

	// Every created container (pause + mysql + job) must be torn down at
	// claim end via CloseScopes -> hostBackend.CloseScopes -> pod.CloseAll.
	assert.Len(t, f.removed, 3, "all claim-pod containers removed at claim end")
}

// TestExecuteRun_Isolated_RuntimeMissing_FailsRunNoContainer verifies that
// when the isolated claim's container runtime cannot be resolved,
// executeRun's failClaim path fires: FinishRun(Failed) is called, no
// container is ever created, and the actionable reason is shipped into the
// run's own logs (stepIndex -1) via AppendLogBulk rather than staying only in
// the agent's local slog.
func TestExecuteRun_Isolated_RuntimeMissing_FailsRunNoContainer(t *testing.T) {
	const agentID = "isolated-runtime-missing-agent"
	const runID = "run-isolated-runtime-missing"

	h := newIsolatedHarness()
	srv := newIsolatedServer(t, agentID, h)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}
	// Seam: pre-seed containerRuntime() to fail without touching crt.Detect
	// (mirrors agent_scope_test.go's runtimeOnce.Do(func(){}) pattern, but
	// here the resolved value is left nil and an error is set instead).
	wantErr := errors.New("no container runtime available (looked for [docker podman nerdctl wslc container])")
	a.runtimeOnce.Do(func() {})
	a.runtimeErr = wantErr

	claim := api.ClaimResponse{
		Native:      false,
		RunID:       runID,
		JobName:     "test-isolated-runtime-missing",
		PodTemplate: mysqlTemplate(),
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, Name: "s1", Run: "echo hi"}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.True(t, h.finishCalled)
	assert.Equal(t, "Failed", h.finishStatus)

	lines := h.logsByStep[-1]
	require.NotEmpty(t, lines, "expected an actionable system log line (stepIndex -1) explaining the failure")
	found := false
	for _, l := range lines {
		if l.Stream == "stderr" && l.StepIndex == -1 &&
			strings.Contains(l.Line, "container runtime") && strings.Contains(l.Line, wantErr.Error()) {
			found = true
		}
	}
	assert.True(t, found, "expected a system log line naming the missing-runtime reason, got: %+v", lines)
}

// TestExecuteRun_Isolated_PodStartFailure_TeardownAndFailsRun verifies that
// when pod.Start fails partway through building the claim pod (pause
// succeeds, the first real container fails), executeRun's failClaim path
// fires: FinishRun(Failed), the actionable reason reaches the run's own logs,
// and every container created before the failure (the pause container at
// minimum) is removed — pod.Start's own closeAllLocked plus the explicit
// pod.CloseAll(...) in executeRun's failure branch, which must tolerate a
// second (idempotent) teardown of an already-emptied pod.
func TestExecuteRun_Isolated_PodStartFailure_TeardownAndFailsRun(t *testing.T) {
	const agentID = "isolated-pod-start-fail-agent"
	const runID = "run-isolated-pod-start-fail"

	h := newIsolatedHarness()
	srv := newIsolatedServer(t, agentID, h)

	f := &failOnNthCreateRT{failAt: 2} // 1st call (pause) ok, 2nd call (mysql) fails
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok"), PauseImage: "pause:img", RunnerImage: "runner:img"}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = f

	claim := api.ClaimResponse{
		Native:      false,
		RunID:       runID,
		JobName:     "test-isolated-pod-start-fail",
		PodTemplate: mysqlTemplate(),
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, Name: "s1", Run: "echo hi"}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.True(t, h.finishCalled)
	assert.Equal(t, "Failed", h.finishStatus)

	lines := h.logsByStep[-1]
	require.NotEmpty(t, lines, "expected an actionable system log line (stepIndex -1) explaining the failure")
	found := false
	for _, l := range lines {
		if l.Stream == "stderr" && l.StepIndex == -1 && strings.Contains(l.Line, "claim pod construction failed") {
			found = true
		}
	}
	assert.True(t, found, "expected a system log line naming the pod-start failure, got: %+v", lines)

	// Every already-created container (the pause container) must have been
	// removed. pod.Start's internal closeAllLocked already tears it down on
	// failure; executeRun's failClaim branch then calls pod.CloseAll again,
	// which must be a harmless no-op the second time (idempotent double
	// teardown tolerated per the task spec).
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Len(t, f.created, 1, "pause created; mysql create attempted (2nd call) but failed before being recorded")
	assert.Equal(t, 2, f.calls, "both the pause and the failing mysql create were attempted")
	assert.Len(t, f.removed, 1, "the pause container was torn down; double-teardown didn't duplicate removals")
	assert.Equal(t, "c0", f.removed[0])
}

// TestExecuteRun_Isolated_MissingPauseImage_FailsRunNoContainer verifies that
// an isolated (Native:false) claim on an agent whose PauseImage was never
// configured (e.g. an Agent built via agent.New, which — unlike cmd/unified-cd-agent —
// does not default it) fails the run fast with an actionable reason, instead of
// letting the claim pod try to start a pause container with an empty image and
// surfacing a cryptic `docker run -d ...: exit status 125`. The guard must fire
// before any container is created.
func TestExecuteRun_Isolated_MissingPauseImage_FailsRunNoContainer(t *testing.T) {
	const agentID = "isolated-empty-pause-agent"
	const runID = "run-isolated-empty-pause"

	h := newIsolatedHarness()
	srv := newIsolatedServer(t, agentID, h)

	// The container runtime resolves fine; the misconfiguration is the empty
	// PauseImage. RunnerImage is set so this isolates the pause-image case.
	f := &podFakeRT{}
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok"), RunnerImage: "runner:img"}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = f

	claim := api.ClaimResponse{
		Native:  false,
		RunID:   runID,
		JobName: "test-isolated-empty-pause",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, Name: "s1", Run: "echo hi"}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.True(t, h.finishCalled)
	assert.Equal(t, "Failed", h.finishStatus)

	lines := h.logsByStep[-1]
	require.NotEmpty(t, lines, "expected an actionable system log line (stepIndex -1)")
	found := false
	for _, l := range lines {
		if l.Stream == "stderr" && l.StepIndex == -1 &&
			strings.Contains(l.Line, "pause image") && strings.Contains(l.Line, "native: true") {
			found = true
		}
	}
	assert.True(t, found, "expected a system log line naming the missing pause image, got: %+v", lines)

	assert.Empty(t, f.created, "no claim-pod container should be created when the pause image is unconfigured")
}

// TestExecuteRun_Isolated_MissingRunnerImage_FailsWhenPrimaryInjected verifies
// that when the claim pod would inject the default "job" primary (no podTemplate
// job container) but the agent's RunnerImage is unconfigured, the run fails fast
// with an actionable reason rather than starting the primary with an empty image.
func TestExecuteRun_Isolated_MissingRunnerImage_FailsWhenPrimaryInjected(t *testing.T) {
	const agentID = "isolated-empty-runner-agent"
	const runID = "run-isolated-empty-runner"

	h := newIsolatedHarness()
	srv := newIsolatedServer(t, agentID, h)

	// Pause image configured; runner image is not. With no podTemplate the claim
	// pod injects the default "job" primary from RunnerImage.
	f := &podFakeRT{}
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok"), PauseImage: "pause:img"}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = f

	claim := api.ClaimResponse{
		Native:  false,
		RunID:   runID,
		JobName: "test-isolated-empty-runner",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, Name: "s1", Run: "echo hi"}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Equal(t, "Failed", h.finishStatus)
	lines := h.logsByStep[-1]
	found := false
	for _, l := range lines {
		if l.Stream == "stderr" && l.StepIndex == -1 &&
			strings.Contains(l.Line, "runner image") && strings.Contains(l.Line, "native: true") {
			found = true
		}
	}
	assert.True(t, found, "expected a system log line naming the missing runner image, got: %+v", lines)
	assert.Empty(t, f.created, "no claim-pod container should be created when the runner image is unconfigured")
}

// TestExecuteRun_Isolated_EmptyRunnerImage_OKWhenPodTemplateSuppliesJob pins the
// conditional nature of the runner-image guard: when the podTemplate provides
// its own "job" container, RunnerImage is unused and may be empty — the claim
// must still run. Guards against the guard becoming an over-broad blanket check.
func TestExecuteRun_Isolated_EmptyRunnerImage_OKWhenPodTemplateSuppliesJob(t *testing.T) {
	const agentID = "isolated-runner-optional-agent"
	const runID = "run-isolated-runner-optional"

	h := newIsolatedHarness()
	srv := newIsolatedServer(t, agentID, h)

	f := &podFakeRT{}
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok"), PauseImage: "pause:img"}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = f

	// podTemplate supplies the "job" primary, so RunnerImage is not needed.
	jobTemplate := &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "job", "image": "golang:1.22"},
		},
	}}
	claim := api.ClaimResponse{
		Native:      false,
		RunID:       runID,
		JobName:     "test-isolated-runner-optional",
		PodTemplate: jobTemplate,
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, Name: "s1", Run: "echo hi"}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Equal(t, "Succeeded", h.finishStatus, "podTemplate supplies the job container; empty RunnerImage is fine")
	require.Len(t, f.created, 2, "pause + podTemplate-supplied job (no injected primary)")
}

// failOnNthCreateRT wraps podFakeRT's recording behavior but fails Create on
// the failAt'th call (1-indexed), simulating a claim pod whose pause
// container starts fine but whose first real podTemplate container fails to
// start.
type failOnNthCreateRT struct {
	podFakeRT
	failAt int
	mu     sync.Mutex
	calls  int
}

func (f *failOnNthCreateRT) Create(ctx context.Context, s crt.CreateSpec) (crt.ContainerHandle, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if n == f.failAt {
		return crt.ContainerHandle{}, errors.New("simulated container create failure")
	}
	return f.podFakeRT.Create(ctx, s)
}
