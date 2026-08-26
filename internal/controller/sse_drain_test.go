package controller

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/require"
)

// TestAPI_RunEvents_SSE_LiveDrainDeliversBatchLargerThanCap is the
// regression test for the final-branch-review Finding 3: before
// handleRunEvents' redrain loop, a single AppendLogs batch carrying more
// lines than sseDrainLimit left the remainder undelivered to a connected SSE
// client whenever that batch was the run's last one — there is no further
// notification to trigger a second drain, so the tail was silently dropped
// from the live stream (a page reload still showed everything; only the
// live view was short).
//
// This drives the REAL live-notify path, not the initial backfill the other
// SSE tests in api_runs_test.go exercise: the run is left Running (not
// finished) when the client connects, so handleRunEvents falls through past
// the terminal-status short-circuit into ListenForNotify. All n lines are
// appended in ONE AppendLogs call — producing exactly one pg_notify — with n
// greater than the test-shrunk sseDrainLimit, so the fix's redrain loop must
// run more than once within that single notification's callback for every
// line to reach the client.
func TestAPI_RunEvents_SSE_LiveDrainDeliversBatchLargerThanCap(t *testing.T) {
	oldDrain := sseDrainLimit
	sseDrainLimit = 3
	t.Cleanup(func() { sseDrainLimit = oldDrain })

	s, pg := newTestServer(t)
	_, err := pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
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
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)

	const n = 7 // > sseDrainLimit (3): the fix's loop must redrain at least twice
	want := make([]string, n)
	for i := range want {
		want[i] = fmt.Sprintf("drain-line-%d", i)
	}

	var mu sync.Mutex
	seen := map[string]bool{}
	done := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			mu.Lock()
			for _, w := range want {
				if strings.Contains(line, w) {
					seen[w] = true
				}
			}
			complete := len(seen) == len(want)
			mu.Unlock()
			if complete {
				close(done)
				return
			}
		}
	}()

	// Give the server's LISTEN a moment to register before the notify fires:
	// a NOTIFY sent before a listener is registered on that channel is
	// simply lost, it is not queued for a late listener.
	time.Sleep(300 * time.Millisecond)

	lines := make([]store.LogAppend, n)
	now := time.Now().UTC()
	for i, w := range want {
		lines[i] = store.LogAppend{RunID: run.ID, StepIndex: 0, Stream: "stdout", Timestamp: now, Line: w}
	}
	seqs, err := pg.AppendLogs(t.Context(), lines)
	require.NoError(t, err)
	for _, seq := range seqs {
		require.NotZero(t, seq, "run must not be sealed")
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		mu.Lock()
		t.Fatalf("timed out waiting for all %d lines to arrive over SSE; saw %d: %v", len(want), len(seen), seen)
		mu.Unlock()
	}
}
