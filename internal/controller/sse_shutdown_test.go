package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/require"
)

// TestAPI_RunEvents_SSE_EndsOnShutdown proves an open live log stream ends
// promptly once the server starts shutting down. Request contexts are not
// cancelled by http.Server.Shutdown, so without this the stream would hold
// Shutdown until its timeout.
func TestAPI_RunEvents_SSE_EndsOnShutdown(t *testing.T) {
	s, pg := newTestServer(t)
	_, err := pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`), store.JobMeta{})
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "", "", store.RunOrigin{})
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(t.Context(), 10)
	require.NoError(t, err)
	_, err = pg.ClaimNextRun(t.Context(), "agent-1", nil, nil)
	require.NoError(t, err)
	require.NoError(t, pg.MarkRunRunning(t.Context(), run.ID))

	srv := httptest.NewServer(s.Router())
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/runs/"+run.ID+"/events", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The run is still Running, so the handler is parked in its wake-up loop.
	time.Sleep(200 * time.Millisecond)
	s.SetShuttingDown()

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE stream did not end within 2s after SetShuttingDown")
	}
}
