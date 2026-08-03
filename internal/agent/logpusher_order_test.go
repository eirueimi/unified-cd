package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLogPusher_PartialFault_PreservesEmissionOrder proves a flush pass does
// not send a newer batch while an older one is still queued.
//
// The regression it guards is a controller fault that fails only SOME requests
// (a flapping upstream, a partially-drained load balancer). The old
// flushLocked retried the whole backlog in one pass, CONTINUED past a failed
// batch, and then sent the current buffer regardless — so a newer batch landed
// while an older one was still pending. The controller assigns seq on arrival,
// so the stored log was permanently out of emission order for every read
// surface (tail, archive, paged read, search all order by seq), with no marker
// and no duplicates to make it visible.
//
// The fake client here fails exactly the middle of three batches, which is the
// minimal shape of that fault.
func TestLogPusher_PartialFault_PreservesEmissionOrder(t *testing.T) {
	const (
		modeFailAll   = "fail-all" // nothing gets through
		modeFailOnlyA = "fail-a"   // only the batch already queued keeps failing
		modeOK        = "ok"       // everything gets through
	)

	var mu sync.Mutex
	mode := modeFailAll
	var received []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		if err := json.NewDecoder(r.Body).Decode(&reqs); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch mode {
		case modeFailAll:
			http.Error(w, "partition", http.StatusInternalServerError)
			return
		case modeFailOnlyA:
			for _, req := range reqs {
				if strings.HasPrefix(req.Line, "A-") {
					http.Error(w, "partial fault", http.StatusInternalServerError)
					return
				}
			}
		}
		for _, req := range reqs {
			received = append(received, req.Line)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	setMode := func(m string) {
		mu.Lock()
		mode = m
		mu.Unlock()
	}
	flush := func(p *LogPusher, ctx context.Context) {
		p.mu.Lock()
		p.flushLocked(ctx)
		p.mu.Unlock()
	}

	client := NewClient(srv.URL, "tok")
	p := NewLogPusher(client, "a1", "run1", 0, "stdout")
	ctx := t.Context()

	// Batch A fails and is queued in pending.
	_, _ = p.Write([]byte("A-1\nA-2\n"))
	flush(p, ctx)
	p.mu.Lock()
	pendingAfterA := len(p.pending)
	p.mu.Unlock()
	require.Equal(t, 1, pendingAfterA, "sanity check: batch A should be queued after the failed send")

	// The fault now only affects batch A. Batches B and C WOULD succeed if the
	// pusher offered them — and that is exactly what it must not do while A is
	// still owed to the controller.
	setMode(modeFailOnlyA)
	_, _ = p.Write([]byte("B-1\n"))
	flush(p, ctx)
	_, _ = p.Write([]byte("C-1\n"))
	flush(p, ctx)

	mu.Lock()
	duringFault := append([]string(nil), received...)
	mu.Unlock()
	assert.Empty(t, duringFault,
		"no batch may land while an older batch is still pending; anything here has overtaken batch A and taken a lower seq")

	// The fault clears; the whole backlog drains in emission order.
	setMode(modeOK)
	flush(p, ctx)

	mu.Lock()
	got := append([]string(nil), received...)
	mu.Unlock()

	assert.Equal(t, []string{"A-1", "A-2", "B-1", "C-1"}, got,
		"stored order must equal emission order (no inversions, no drops, no duplicates)")

	p.mu.Lock()
	pendingAtEnd := len(p.pending)
	p.mu.Unlock()
	assert.Equal(t, 0, pendingAtEnd, "the backlog should be fully drained once the fault clears")
}

// TestLogPusher_BlockedBacklog_StillBoundsMemory proves that aborting a flush
// pass does NOT leave the current buffer undrained.
//
// Preserving order by simply returning early from flushLocked would also
// preserve it — but p.buf has no cap, whereas p.pending carries
// maxPendingBytes, drop-oldest eviction and the droppedLines accounting that
// feeds the drop marker. An early return would therefore convert a bounded,
// accounted 1MB backlog into unbounded agent memory growth for the length of
// the outage, and silence the drop count. The buffer must keep draining INTO
// pending; it just must not go onto the wire ahead of the backlog.
func TestLogPusher_BlockedBacklog_StillBoundsMemory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "always fail", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "tok")
	p := NewLogPusher(client, "a1", "run1", 0, "stdout")
	p.maxPendingBytes = 32 // tiny cap so eviction engages quickly

	ctx := t.Context()
	for i := 0; i < 50; i++ {
		_, _ = p.Write([]byte("a log line that is long enough to matter\n"))
		p.mu.Lock()
		p.flushLocked(ctx)
		bufLen := p.buf.Len()
		p.mu.Unlock()
		require.Equal(t, 0, bufLen, "the buffer must be drained into pending even when the backlog is blocked")
	}

	p.mu.Lock()
	pendingBytes := p.pendingSizeBytes()
	batches := len(p.pending)
	dropped := p.droppedLines
	p.mu.Unlock()

	assert.LessOrEqual(t, batches, 2, "drop-oldest eviction must still bound the backlog")
	assert.LessOrEqual(t, pendingBytes, 2*p.maxPendingBytes, "pending must stay near its byte cap")
	assert.Greater(t, dropped, 0, "evicted lines must still be counted for the drop marker")
}

// TestLogPusher_StartAutoFlush_DeadlineUnblocksWriter proves an auto-flush
// tick cannot freeze the step that is producing the logs.
//
// StartAutoFlush holds p.mu for the whole pass and Write blocks on that mutex,
// so with no per-flush deadline a controller that accepts the connection and
// never answers stalls the step's own process for as long as the HTTP client
// allows (60s per request, and a pass can contain several). The measured case
// was a 176.3s freeze of a step whose real work had nothing to do with
// logging. With a per-flush deadline the writer gets the lock back after the
// deadline instead.
func TestLogPusher_StartAutoFlush_DeadlineUnblocksWriter(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Black hole: accept and never answer. Bounded only so this test
		// cannot hang forever when the deadline is missing.
		select {
		case <-release:
		case <-time.After(3 * time.Second):
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	defer close(release)

	prev := logPusherAutoFlushTimeout
	logPusherAutoFlushTimeout = 150 * time.Millisecond
	t.Cleanup(func() { logPusherAutoFlushTimeout = prev })

	client := NewClient(srv.URL, "tok")
	p := NewLogPusher(client, "a1", "run1", 0, "stdout")
	_, _ = p.Write([]byte("first line\n"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartAutoFlush(ctx, 20*time.Millisecond)

	// Let a tick take the lock and block inside the request.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	_, _ = p.Write([]byte("the step keeps working\n"))
	blocked := time.Since(start)

	assert.Less(t, blocked, time.Second,
		"Write (and therefore the step's stdout pipe) must not be held by an auto-flush pass beyond the per-flush deadline; blocked for %s", blocked)
}
