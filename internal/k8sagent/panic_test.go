package k8sagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
)

// TestRun_DispatchPanicFailsRunNotProcess verifies the k8s agent's dispatch
// goroutine guard (audit item 4, defense-in-depth): a panic inside a.dispatch
// (executeRun's call graph — anything above the step level, which pipeline.go's
// runOne already recovers) must be caught by Run's per-claim goroutine so it
// fails only that run, instead of crashing the whole agent process and taking
// every other in-flight run down with it. Models the claim/dispatch seam used
// by TestRun_DrainWaitsForInflight / TestRun_SemaphoreBoundsConcurrency in
// drain_test.go (newK8sAgentForTest, a.dispatch override), extended with a
// finish handler so the recovered panic's Failed report can be observed.
func TestRun_DispatchPanicFailsRunNotProcess(t *testing.T) {
	const agentID = "k8s-panic-agent"
	const runID = "r"

	var remaining atomic.Int32
	remaining.Store(1) // exactly one claim

	finishCh := make(chan string, 1)
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("POST /api/v1/agents/register", ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/heartbeat", ok)
	mux.HandleFunc("DELETE /api/v1/agents/"+agentID, ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/reconcile", func(w http.ResponseWriter, _ *http.Request) {
		orchestrateWriteJSON(w, map[string]int{"failedRuns": 0})
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/claim", func(w http.ResponseWriter, _ *http.Request) {
		if remaining.Add(-1) >= 0 {
			orchestrateWriteJSON(w, api.ClaimResponse{RunID: runID})
			return
		}
		orchestrateWriteJSON(w, api.ClaimResponse{})
	})
	// failRun's best-effort System log line (stepIndex -1); response content
	// doesn't matter, only that it doesn't block FinishRun below.
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/-1/logs/bulk", ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishCh <- body.Status:
		default:
		}
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := agentlib.NewClient(srv.URL, "tok")
	a := newK8sAgentForTest(t, Config{AgentID: agentID, Namespace: "ns", MaxConcurrent: 1}, client)
	a.dispatch = func(ctx context.Context, c api.ClaimResponse) {
		panic("kaboom in dispatch")
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		_ = a.Run(ctx) // must not crash the test process
		close(runDone)
	}()

	select {
	case status := <-finishCh:
		if status != "Failed" {
			t.Fatalf("expected the run to be reported Failed after the dispatch panic, got %q", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun was not called after the dispatch panic")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after cancel (dispatch goroutine may be stuck)")
	}
}
