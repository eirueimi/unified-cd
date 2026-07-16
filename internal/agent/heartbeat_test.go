package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
)

// TestStartHeartbeat_DoneChannelJoinsGoroutine verifies StartHeartbeat returns
// a channel that closes only after its goroutine has fully stopped — so a caller
// can join it and be sure no further heartbeat fires. Without this join the
// goroutine outlives its starter (cancel only signals it asynchronously), which
// is exactly what let a stray beat land after Agent.Run returned.
func TestStartHeartbeat_DoneChannelJoinsGoroutine(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agents/a1/heartbeat" {
			atomic.AddInt32(&hits, 1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "t")
	ctx, cancel := context.WithCancel(context.Background())
	done := StartHeartbeat(ctx, c, "a1", 20*time.Millisecond, func() []string { return nil })

	time.Sleep(80 * time.Millisecond) // a few beats
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartHeartbeat goroutine did not exit after ctx cancel")
	}

	// The goroutine has exited, so it starts no new heartbeat. One beat may have
	// been in flight when ctx was cancelled — the client aborts it, but the
	// server can still count that request a touch after done closes — so absorb
	// that single straggler with a short settle, then assert the count is truly
	// stable. A leaked (un-joined) goroutine would keep incrementing past it.
	time.Sleep(100 * time.Millisecond)
	baseline := atomic.LoadInt32(&hits)
	time.Sleep(100 * time.Millisecond)
	if grown := atomic.LoadInt32(&hits); grown != baseline {
		t.Fatalf("heartbeat kept firing after the goroutine exited: %d -> %d", baseline, grown)
	}
}

func TestStartHeartbeat_TicksUntilCtxDone(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agents/a1/heartbeat" {
			atomic.AddInt32(&hits, 1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "t")
	ctx, cancel := context.WithCancel(context.Background())
	done := StartHeartbeat(ctx, c, "a1", 20*time.Millisecond, func() []string { return nil })
	time.Sleep(120 * time.Millisecond)
	if got := atomic.LoadInt32(&hits); got < 3 {
		t.Fatalf("expected several heartbeats, got %d", got)
	}
	// After cancel the heartbeats must stop. Join the goroutine, then absorb any
	// single in-flight beat (the server can count an aborted request just after
	// the goroutine exits) with a short settle before asserting the count is
	// stable. hits only ever increments, so the equality check is a real assertion.
	cancel()
	<-done
	time.Sleep(100 * time.Millisecond)
	afterCancel := atomic.LoadInt32(&hits)
	time.Sleep(100 * time.Millisecond)
	if grown := atomic.LoadInt32(&hits); grown != afterCancel {
		t.Fatalf("heartbeats continued after cancel: %d -> %d", afterCancel, grown)
	}
}

// TestStartHeartbeat_SendsActiveRunIDsFromProvider verifies each beat calls
// the provider and forwards its result to client.Heartbeat, so an in-flight
// run shows up in the heartbeat body (the foundation for the controller's
// lost-claim reconcile).
func TestStartHeartbeat_SendsActiveRunIDsFromProvider(t *testing.T) {
	var lastBody atomic.Value // api.HeartbeatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got api.HeartbeatRequest
		_ = json.NewDecoder(r.Body).Decode(&got)
		lastBody.Store(got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "t")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := StartHeartbeat(ctx, c, "a1", 20*time.Millisecond, func() []string { return []string{"run-1", "run-2"} })
	defer func() { cancel(); <-done }()

	deadline := time.After(2 * time.Second)
	for {
		if v, ok := lastBody.Load().(api.HeartbeatRequest); ok && len(v.ActiveRunIDs) == 2 {
			if v.ActiveRunIDs[0] == "run-1" && v.ActiveRunIDs[1] == "run-2" {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatal("heartbeat never carried the provider's active run IDs")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
