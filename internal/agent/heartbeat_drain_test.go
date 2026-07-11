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

// TestAgent_HeartbeatContinuesDuringDrain is a regression test for the heartbeat
// goroutine being bound to the wrong context (issue #27). The heartbeat must keep
// firing while an in-flight run is still draining after claimCtx is cancelled
// (SIGTERM/cordon) — otherwise the stuck-run reaper would Fail a healthy draining
// run once last_seen_at goes stale.
func TestAgent_HeartbeatContinuesDuringDrain(t *testing.T) {
	// Shorten the heartbeat interval for the test.
	oldInterval := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	defer func() { heartbeatInterval = oldInterval }()

	var hits int32
	stepStarted := make(chan struct{}, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", registerHandler)
	mux.HandleFunc("POST /api/v1/agents/hb-drain/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNoContent)
	})

	claimCount := 0
	var mu sync.Mutex
	mux.HandleFunc("POST /api/v1/agents/hb-drain/claim", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := claimCount
		claimCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 0 {
			// Long-running step so the run is still in-flight during drain.
			json.NewEncoder(w).Encode(claimResp("run-hb", "sleep 1")) //nolint:errcheck
		} else {
			<-r.Context().Done()
			json.NewEncoder(w).Encode(api.ClaimResponse{}) //nolint:errcheck
		}
	})
	mux.HandleFunc("POST /api/v1/agents/hb-drain/steps", func(w http.ResponseWriter, r *http.Request) {
		select {
		case stepStarted <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// Catch-all (finish, logs, get run, etc.) returns 204.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	claimCtx, cancelClaim := context.WithCancel(context.Background())
	defer cancelClaim()

	a := &Agent{
		ID:            "hb-drain",
		Client:        NewClient(srv.URL, "tok"),
		MaxConcurrent: 1,
		// Large drain timeout so runCtx (and thus the heartbeat) survives the drain
		// window the test observes.
		DrainTimeout: 5 * time.Second,
	}

	done := make(chan error, 1)
	go func() { done <- a.Run(claimCtx) }()

	// Wait for the step to start, then simulate SIGTERM (cordon+drain).
	select {
	case <-stepStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("step did not start")
	}
	cancelClaim()

	// Record the heartbeat count at the moment drain begins, then wait several
	// heartbeat intervals and assert the count grew — i.e. heartbeats did NOT
	// stop when claimCtx was cancelled.
	atStart := atomic.LoadInt32(&hits)
	time.Sleep(200 * time.Millisecond) // ~10 intervals
	atEnd := atomic.LoadInt32(&hits)
	if atEnd <= atStart {
		t.Fatalf("heartbeats stopped during drain: count %d did not grow past %d", atEnd, atStart)
	}

	// Let the run finish and Run() return; heartbeats should then stop.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not return after drain")
	}
	// Run has returned, which joins the heartbeat goroutine. A single beat may
	// have been in flight at shutdown (the server can count an aborted request a
	// touch later), so absorb that with a settle before capturing the baseline,
	// then assert no further growth — a leaked goroutine would keep beating.
	time.Sleep(100 * time.Millisecond)
	afterReturn := atomic.LoadInt32(&hits)
	time.Sleep(100 * time.Millisecond)
	if grown := atomic.LoadInt32(&hits); grown != afterReturn {
		t.Fatalf("heartbeats leaked after full shutdown: %d -> %d", afterReturn, grown)
	}
}
