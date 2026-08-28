package k8sagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPodBindingSet_AddRemoveSnapshot is a plain unit test of the small
// mutex-guarded map itself, independent of K8sAgent wiring.
func TestPodBindingSet_AddRemoveSnapshot(t *testing.T) {
	s := newPodBindingSet()
	assert.Empty(t, s.Snapshot())

	s.Add("run-1", api.PodBinding{PodName: "pod-1", PodUID: "uid-1"})
	got := s.Snapshot()
	require.Len(t, got, 1)
	assert.Equal(t, api.PodBinding{PodName: "pod-1", PodUID: "uid-1"}, got["run-1"])

	// Snapshot returns a copy: mutating it must not affect the set.
	got["run-1"] = api.PodBinding{PodName: "tampered", PodUID: "tampered"}
	assert.Equal(t, "pod-1", s.Snapshot()["run-1"].PodName)

	s.Remove("run-1")
	assert.Empty(t, s.Snapshot())
}

// TestPodBindingSet_NilReceiverIsSafe pins the defensive nil-safety podbindings.go
// documents: several existing tests build a *K8sAgent via a bare struct
// literal rather than NewK8sAgent, leaving podBindings nil, and executeRun
// must not panic against one.
func TestPodBindingSet_NilReceiverIsSafe(t *testing.T) {
	var s *podBindingSet
	assert.NotPanics(t, func() {
		s.Add("run-1", api.PodBinding{PodName: "pod-1", PodUID: "uid-1"})
		s.Remove("run-1")
	})
	assert.Empty(t, s.Snapshot())
}

// TestExecuteRun_RecordsPodBinding verifies executeRun's non-pool path
// records the created Pod's name and UID into a.podBindings for the claim's
// RunID — the write side the store-credentials broker's run-binding
// enforcement (internal/controller/api_store_credentials.go) depends on via
// the heartbeat (see api.HeartbeatRequest.PodBindings). fakePM.CreatePod
// simulates a real API server assigning both Name and UID at creation.
func TestExecuteRun_RecordsPodBinding(t *testing.T) {
	const agentID = "k8s-podbind-1"
	const runID = "run-podbind-1"

	finishCh := make(chan api.RunStatus, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		select {
		case finishCh <- api.RunStatus(body["status"]):
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/outputs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pm := &fakePM{}
	ex := &fakeExec{stdout: "ok\n"}
	agentClient := agentlib.NewClient(srv.URL, "tok")

	cfg := Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04"}
	// A literal, not NewK8sAgent: pm/ex are fakes satisfying the internal
	// podManager/stepExecutor interfaces (see fakepm_test.go), not the
	// concrete *PodManager/*Executor NewK8sAgent requires — the same reason
	// agent_env_test.go's TestExecuteRun_DefaultStep_EnvInjected builds one
	// this way. podBindings is set explicitly (NewK8sAgent's only job here)
	// since this test asserts on it directly.
	a := &K8sAgent{cfg: cfg, client: agentClient, pm: pm, exec: ex, podBindings: newPodBindingSet()}

	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "podbind-test",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "build", Run: "echo hi"}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.executeRun(ctx, claim)

	select {
	case status := <-finishCh:
		require.Equal(t, api.RunSucceeded, status)
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun not called")
	}

	// executeRun itself never removes the binding — that happens in
	// k8sClaimLoop's dispatch goroutine, which this test bypasses by calling
	// executeRun directly (mirrors every other direct-executeRun test in this
	// package) — so the binding is still present to assert on here.
	got := a.podBindings.Snapshot()
	require.Contains(t, got, runID)
	assert.Equal(t, pm.createdNm, got[runID].PodName)
	assert.NotEmpty(t, got[runID].PodUID, "the created Pod's UID must be recorded, not left empty")
}
