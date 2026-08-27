package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/require"
)

// TestAPI_RunEvents_SSE_TwoViewersOneRun_ShareOneListenConnection is the
// direct regression test for the whole point of this change: before the
// shared logNotifyHub, handleRunEvents called (*store.Postgres).
// ListenForNotify once per SSE request, each call holding its own
// listenPool connection for the life of the stream. Two browser tabs
// watching the SAME run therefore held two connections, not one — this
// test opens exactly that (two concurrent /events streams on one run) and
// asserts the listen pool never needs more than one connection to serve
// both, and stays there for as long as both streams are open.
func TestAPI_RunEvents_SSE_TwoViewersOneRun_ShareOneListenConnection(t *testing.T) {
	pg := store.NewTestPostgres(t)
	_, err := pg.UpsertBootstrapPAT(context.Background(), "test-bootstrap", HashToken("secret"))
	require.NoError(t, err)
	s := NewServer(Config{}, pg)
	// Stops the shared log-notify listener goroutine this test's SSE streams
	// start (see (*Server).Close). Ordered ahead of NewTestPostgres' own
	// pg.Close() cleanup (registered earlier, inside NewTestPostgres above,
	// so it runs later under t.Cleanup's LIFO order) so the listener is
	// cancelled before the pool it's reading from closes underneath it.
	t.Cleanup(s.Close)

	_, err = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(t.Context(), 10)
	require.NoError(t, err)
	_, err = pg.ClaimNextRun(t.Context(), "agent-1", nil)
	require.NoError(t, err)
	// Left Running (not terminal): the SSE handler must fall through the
	// terminal-status short-circuit and actually enter the live-notify
	// path for this test to mean anything.
	require.NoError(t, pg.MarkRunRunning(t.Context(), run.ID))

	srv := httptest.NewServer(s.Router())
	t.Cleanup(srv.Close)

	openSSEStream := func() *http.Response {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/runs/"+run.ID+"/events", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return resp
	}

	resp1 := openSSEStream()
	t.Cleanup(func() { _ = resp1.Body.Close() })
	resp2 := openSSEStream()
	t.Cleanup(func() { _ = resp2.Body.Close() })

	require.Eventually(t, func() bool {
		return pg.PoolStats()["listen"].AcquiredConns == 1
	}, 5*time.Second, 20*time.Millisecond,
		"expected exactly one listenPool connection to serve both viewers of the same run")

	// Hold a beat and re-check: a flaky implementation could transiently
	// pass through 1 on its way to 2 (e.g. a second connection racing to
	// come up) — require.Eventually alone would not catch that.
	time.Sleep(300 * time.Millisecond)
	require.EqualValues(t, 1, pg.PoolStats()["listen"].AcquiredConns,
		"listenPool connections drifted away from 1 while both viewers were still connected")
}

// TestAPI_RunEvents_SSE_DisconnectRemovesSubscriber is the end-to-end
// counterpart to log_notify_test.go's hub-level unsubscribe tests: those
// prove logNotifyHub.subscribe/unsubscribe are correct in isolation, but not
// that handleRunEvents actually calls unsubscribe when a REAL client
// disconnects, through the REAL deferred-cleanup path (r.Context().Done()
// firing on connection close, not a direct unsubscribe() call). A leak here
// — the subscriber entry surviving disconnect — is exactly the "slow memory
// leak plus wake-ups delivered forever to nobody" failure the task calls
// out, and it is invisible to the two-viewers-one-connection test above,
// which only ever checks state while both viewers are still connected.
func TestAPI_RunEvents_SSE_DisconnectRemovesSubscriber(t *testing.T) {
	pg := store.NewTestPostgres(t)
	_, err := pg.UpsertBootstrapPAT(context.Background(), "test-bootstrap", HashToken("secret"))
	require.NoError(t, err)
	s := NewServer(Config{}, pg)
	t.Cleanup(s.Close)

	_, err = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(t.Context(), 10)
	require.NoError(t, err)
	_, err = pg.ClaimNextRun(t.Context(), "agent-1", nil)
	require.NoError(t, err)
	require.NoError(t, pg.MarkRunRunning(t.Context(), run.ID))

	srv := httptest.NewServer(s.Router())
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/runs/"+run.ID+"/events", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	subCount := func() int {
		s.logNotify.mu.Lock()
		defer s.logNotify.mu.Unlock()
		return len(s.logNotify.subs[run.ID])
	}

	require.Eventually(t, func() bool { return subCount() == 1 }, 5*time.Second, 20*time.Millisecond,
		"expected the connected viewer to register exactly one subscriber for its run")

	require.NoError(t, resp.Body.Close())

	require.Eventually(t, func() bool { return subCount() == 0 }, 5*time.Second, 20*time.Millisecond,
		"subscriber was not removed from the hub after the client disconnected — a leaked registration, "+
			"not just a stopped delivery, which is the failure mode this test exists to catch")
}
