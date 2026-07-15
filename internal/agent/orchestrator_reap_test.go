package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
)

// reapServer is a fake controller (modeled on newRetryServer) whose GET run
// endpoint reports the status currently in *status, and which counts FinishRun
// calls. It registers the routes a native RunClaim actually hits.
func reapServer(t *testing.T, agentID string, status *atomic.Value, finishCalls *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	noContent := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	mux.HandleFunc("POST /api/v1/agents/register", noContent)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", noContent)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/logs/bulk", ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/outputs", ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/outputs", ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", ok) // system (stepIndex -1) log line, if any
	mux.HandleFunc("GET /api/v1/runs/{runId}", func(w http.ResponseWriter, r *http.Request) {
		s, _ := status.Load().(string)
		if s == "" {
			s = string(api.RunRunning)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Run{ID: r.PathValue("runId"), Status: api.RunStatus(s)})
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/finish", func(w http.ResponseWriter, _ *http.Request) {
		finishCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestRunClaim_ReapedByMaster_SkipsTerminalFinish: the controller reports the run
// Failed out-of-band while a step is still running; the poller cancels the run and
// RunClaim does NOT send its own terminal FinishRun (the controller is authoritative).
func TestRunClaim_ReapedByMaster_SkipsTerminalFinish(t *testing.T) {
	prevPoll := CancelPollInterval
	CancelPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { CancelPollInterval = prevPoll })

	const agentID = "reap-agent"
	var status atomic.Value
	status.Store(string(api.RunFailed)) // reaped from the first poll
	var finishCalls atomic.Int32

	srv := reapServer(t, agentID, &status, &finishCalls)
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   "run-reap",
		JobName: "reap-job",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, Name: "blocker", Run: "sleep 2"}},
		},
	}

	done := make(chan struct{})
	go func() { a.executeRun(context.Background(), claim, t.TempDir()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("executeRun did not return after the run was reaped")
	}

	if finishCalls.Load() != 0 {
		t.Fatalf("a master-reaped run must not send its own terminal FinishRun, got %d calls", finishCalls.Load())
	}
}

// TestRunClaim_Cancelled_StillFinishes is the contrast: an out-of-band Cancelled
// status still results in a normal terminal FinishRun(Cancelled) — the reaped-skip
// applies only to non-Cancelled terminal statuses.
func TestRunClaim_Cancelled_StillFinishes(t *testing.T) {
	prevPoll := CancelPollInterval
	CancelPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { CancelPollInterval = prevPoll })

	const agentID = "reap-agent"
	var status atomic.Value
	status.Store(string(api.RunCancelled))
	var finishCalls atomic.Int32

	srv := reapServer(t, agentID, &status, &finishCalls)
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	claim := api.ClaimResponse{
		Native: true, RunID: "run-cancel", JobName: "cancel-job",
		Stages: []api.ClaimStage{{Step: &api.ClaimStep{Index: 0, Name: "blocker", Run: "sleep 2"}}},
	}
	a.executeRun(context.Background(), claim, t.TempDir())

	if finishCalls.Load() == 0 {
		t.Fatal("a cancelled run must still send FinishRun(Cancelled)")
	}
}

var _ = sync.Mutex{} // guard: keep sync import valid regardless of final edits
