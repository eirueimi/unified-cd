package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
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
	done := StartHeartbeat(ctx, c, "a1", 20*time.Millisecond)

	time.Sleep(80 * time.Millisecond) // a few beats
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartHeartbeat goroutine did not exit after ctx cancel")
	}

	// done is closed => the goroutine has returned; no beat can land after this.
	after := atomic.LoadInt32(&hits)
	time.Sleep(100 * time.Millisecond)
	if grown := atomic.LoadInt32(&hits); grown != after {
		t.Fatalf("heartbeat fired after done closed: %d -> %d", after, grown)
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
	StartHeartbeat(ctx, c, "a1", 20*time.Millisecond)
	time.Sleep(120 * time.Millisecond)
	cancel()
	got := atomic.LoadInt32(&hits)
	if got < 3 {
		t.Fatalf("expected several heartbeats, got %d", got)
	}
	// After cancel, heartbeats must stop. Record the count immediately, wait
	// several intervals, and assert the count did NOT increase. (hits only ever
	// increments, so a strict equality check is a real assertion here.)
	afterCancel := atomic.LoadInt32(&hits)
	time.Sleep(100 * time.Millisecond) // several 20ms intervals
	if grown := atomic.LoadInt32(&hits); grown != afterCancel {
		t.Fatalf("heartbeats continued after cancel: %d -> %d", afterCancel, grown)
	}
}
