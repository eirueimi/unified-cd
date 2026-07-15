package k8sagent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
)

// controllerForWait returns a fake controller whose GET /runs/{id} reports the
// status currently stored in *status, and records whether finish was called.
func controllerForWait(t *testing.T, status *atomic.Value, finishCalled *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/{id}/logs", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /api/v1/runs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		s, _ := status.Load().(string)
		if s == "" {
			s = "Running"
		}
		orchestrateWriteJSON(w, api.Run{ID: "r1", Status: api.RunStatus(s)})
	})
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/finish", func(w http.ResponseWriter, _ *http.Request) {
		finishCalled.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestExecuteRun_WaitFailureFailsRunWithDeadline: a fresh (non-pooled) run whose
// pod never becomes ready is failed (retried) and its pod deleted, and the wait
// carried a deadline.
func TestExecuteRun_WaitFailureFailsRunWithDeadline(t *testing.T) {
	prevInitial, prevMax := agentlib.RetryInitialWait, agentlib.RetryMaxWait
	agentlib.RetryInitialWait, agentlib.RetryMaxWait = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { agentlib.RetryInitialWait, agentlib.RetryMaxWait = prevInitial, prevMax })

	var status atomic.Value
	var finishCalled atomic.Int32
	srv := controllerForWait(t, &status, &finishCalled)
	client := agentlib.NewClient(srv.URL, "tok")

	pm := &fakePM{waitErr: errors.New("pod stuck pending")}
	a := &K8sAgent{
		cfg:    Config{AgentID: "k8s-1", Namespace: "ns", PodImage: "img", ShimImage: "shim", PodStartTimeout: "50ms"},
		client: client,
		pm:     pm,
	}
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "s", Run: "echo ok"}},
	}}

	a.executeRun(context.Background(), c)

	if !pm.waitHadDeadline {
		t.Fatal("run-pod wait must carry a deadline (PodStartTimeout)")
	}
	if finishCalled.Load() == 0 {
		t.Fatal("a wait failure must fail the run (FinishRun) via failRun")
	}
	if len(pm.deleted) == 0 {
		t.Fatal("the created pod must be deleted on wait failure (no leak)")
	}
}

// TestExecuteRun_MasterTerminalDuringWaitAbandonsWithoutOverride: the run is
// flipped Cancelled by the controller while the pod is still not ready; the wait
// aborts early and executeRun returns WITHOUT overriding the controller status.
func TestExecuteRun_MasterTerminalDuringWaitAbandonsWithoutOverride(t *testing.T) {
	prevPoll := agentlib.CancelPollInterval
	agentlib.CancelPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { agentlib.CancelPollInterval = prevPoll })

	var status atomic.Value
	status.Store("Running")
	var finishCalled atomic.Int32
	srv := controllerForWait(t, &status, &finishCalled)
	client := agentlib.NewClient(srv.URL, "tok")

	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	pm := &fakePM{waitBlock: block} // blocks until ctx is cancelled by the watcher
	a := &K8sAgent{
		cfg:    Config{AgentID: "k8s-1", Namespace: "ns", PodImage: "img", ShimImage: "shim", PodStartTimeout: "10s"},
		client: client,
		pm:     pm,
	}
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "s", Run: "echo ok"}},
	}}

	// Flip the run terminal shortly after executeRun starts waiting.
	go func() {
		time.Sleep(20 * time.Millisecond)
		status.Store("Cancelled")
	}()

	done := make(chan struct{})
	go func() { a.executeRun(context.Background(), c); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executeRun did not return after the run went terminal during the wait")
	}

	if finishCalled.Load() != 0 {
		t.Fatal("must NOT override controller status when the run is already terminal")
	}
	if len(pm.deleted) == 0 {
		t.Fatal("the created pod must still be deleted when abandoning a terminal run")
	}
}
