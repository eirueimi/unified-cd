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
	"k8s.io/client-go/kubernetes/fake"
)

// claimController hands out `total` claims, then empty responses, and accepts
// register/heartbeat/deregister/reconcile. Routes verified against
// internal/agent/client.go: register POST /api/v1/agents/register; reconcile
// POST /api/v1/agents/{id}/runs/reconcile returning {"failedRuns":N}; claim POST
// /api/v1/agents/{id}/claim; heartbeat POST /api/v1/agents/{id}/heartbeat;
// deregister DELETE /api/v1/agents/{id}. Uses stdlib encoding/json (this is
// package k8sagent, so orchestrateWriteJSON is available too — either works).
func claimController(t *testing.T, agentID string, total *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("POST /api/v1/agents/register", ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/heartbeat", ok)
	mux.HandleFunc("DELETE /api/v1/agents/"+agentID, ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/reconcile", func(w http.ResponseWriter, _ *http.Request) {
		orchestrateWriteJSON(w, map[string]int{"failedRuns": 0})
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/claim", func(w http.ResponseWriter, _ *http.Request) {
		if total.Add(-1) >= 0 {
			orchestrateWriteJSON(w, api.ClaimResponse{RunID: "r"})
			return
		}
		orchestrateWriteJSON(w, api.ClaimResponse{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newK8sAgentForTest(t *testing.T, cfg Config, client *agentlib.Client) *K8sAgent {
	t.Helper()
	fakeCS := fake.NewSimpleClientset()
	pm := NewPodManager(fakeCS, cfg.Namespace, "img")
	pool := NewPodPool(fakeCS, cfg.Namespace, pm)
	a := NewK8sAgent(cfg, client, pm, nil, pool)
	return a
}

// TestRun_DrainWaitsForInflight: an in-flight dispatch keeps running under runCtx
// after claimCtx is cancelled, and Run returns only once it completes.
func TestRun_DrainWaitsForInflight(t *testing.T) {
	var remaining atomic.Int32
	remaining.Store(1) // exactly one claim
	srv := claimController(t, "k8s-1", &remaining)
	client := agentlib.NewClient(srv.URL, "tok")

	// Build with a real pm/pool over a fake clientset so Restore/GC no-op cleanly.
	// (If constructing PodPool/PodManager without a cluster is impractical here,
	// use k8s.io/client-go/kubernetes/fake to build the clientset.)
	a := newK8sAgentForTest(t, Config{AgentID: "k8s-1", Namespace: "ns", MaxConcurrent: 5}, client)

	started := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	a.dispatch = func(ctx context.Context, c api.ClaimResponse) {
		close(started)
		<-release // hold the run "in flight"
		finished.Store(true)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = a.Run(ctx); close(runDone) }()

	<-started // a run is in flight
	cancel()  // begin drain (stop claiming)
	time.Sleep(50 * time.Millisecond)
	if finished.Load() {
		t.Fatal("in-flight run should still be running during drain")
	}
	select {
	case <-runDone:
		t.Fatal("Run returned before the in-flight run finished draining")
	default:
	}
	close(release) // let the run complete
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the in-flight run drained")
	}
	if !finished.Load() {
		t.Fatal("in-flight run should have completed under runCtx")
	}
}

// TestRun_SemaphoreBoundsConcurrency: with MaxConcurrent=2, no more than 2
// dispatches run at once.
func TestRun_SemaphoreBoundsConcurrency(t *testing.T) {
	var remaining atomic.Int32
	remaining.Store(6)
	srv := claimController(t, "k8s-1", &remaining)
	client := agentlib.NewClient(srv.URL, "tok")
	a := newK8sAgentForTest(t, Config{AgentID: "k8s-1", Namespace: "ns", MaxConcurrent: 2}, client)

	var cur, max atomic.Int32
	var mu sync.Mutex
	a.dispatch = func(ctx context.Context, c api.ClaimResponse) {
		n := cur.Add(1)
		mu.Lock()
		if n > max.Load() {
			max.Store(n)
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		cur.Add(-1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = a.Run(ctx); close(runDone) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-runDone

	if max.Load() > 2 {
		t.Fatalf("MaxConcurrent=2 must bound concurrency, observed %d", max.Load())
	}
	if max.Load() == 0 {
		t.Fatal("expected some dispatches to run")
	}
}
