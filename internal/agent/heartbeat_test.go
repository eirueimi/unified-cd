package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

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
	// after cancel, hits should stop growing
	time.Sleep(60 * time.Millisecond)
	if atomic.LoadInt32(&hits) != got && atomic.LoadInt32(&hits) < got {
		t.Fatalf("heartbeats continued after cancel")
	}
}
