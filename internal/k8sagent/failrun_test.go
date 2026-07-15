package k8sagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
)

// TestFailRun_RetriesFinishAndLogsReason verifies failRun surfaces its reason
// as a System log line and retries FinishRun past a transient 500.
func TestFailRun_RetriesFinishAndLogsReason(t *testing.T) {
	prevInitial, prevMax := agentlib.RetryInitialWait, agentlib.RetryMaxWait
	agentlib.RetryInitialWait, agentlib.RetryMaxWait = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { agentlib.RetryInitialWait, agentlib.RetryMaxWait = prevInitial, prevMax })

	var finishCalls atomic.Int32
	var mu sync.Mutex
	var gotLogLine string
	var gotFinishStatus string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/steps/{stepIdx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		_ = orchestrateDecodeJSON(r, &reqs)
		mu.Lock()
		if len(reqs) > 0 {
			gotLogLine = reqs[0].Line
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/finish", func(w http.ResponseWriter, r *http.Request) {
		if finishCalls.Add(1) <= 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var req struct {
			Status string `json:"status"`
		}
		_ = orchestrateDecodeJSON(r, &req)
		mu.Lock()
		gotFinishStatus = req.Status
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := agentlib.NewClient(srv.URL, "tok")
	a := &K8sAgent{cfg: Config{AgentID: "k8s-1"}, client: client}

	a.failRun(context.Background(), "r1", "boom: pod did not start")

	mu.Lock()
	defer mu.Unlock()
	if gotLogLine != "boom: pod did not start" {
		t.Fatalf("system log line: got %q", gotLogLine)
	}
	if gotFinishStatus != string(api.RunFailed) {
		t.Fatalf("finish status: got %q, want Failed", gotFinishStatus)
	}
	if finishCalls.Load() < 2 {
		t.Fatalf("expected FinishRun to be retried (>=2 calls), got %d", finishCalls.Load())
	}
}
