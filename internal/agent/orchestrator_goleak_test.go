package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"go.uber.org/goleak"
)

// TestRunClaim_JoinsCancelPoller_NoGoroutineLeak is the deterministic guard for
// RunClaim joining its cancellation poller before returning. The poller is
// spawned inside RunClaim; if the join (pollerWG.Wait) is ever removed, the
// goroutine outlives RunClaim and — depending on scheduling — reads the
// CancelPollInterval package var after the call returns, which only ever
// surfaced probabilistically under `go test -race`. goleak turns that into a
// deterministic failure: any goroutine still alive at return that wasn't there
// before is flagged.
func TestRunClaim_JoinsCancelPoller_NoGoroutineLeak(t *testing.T) {
	// IgnoreCurrent snapshots the goroutines alive now (deferred-call arguments
	// are evaluated at the defer statement, i.e. here), so pre-existing
	// test/runtime goroutines are ignored and only a leak introduced by this
	// test — an un-joined cancel poller — is reported.
	ignore := goleak.IgnoreCurrent()

	const agentID = "goleak-agent"
	const runID = "run-goleak"

	// Minimal controller: GetRun (the poller's only call) reports Running so the
	// poller never self-cancels; everything else (FinishRun, etc.) returns 204.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/runs/") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(api.Run{ID: runID, Status: api.RunRunning})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A native claim with no steps: RunClaim starts the poller, runs an empty
	// pipeline, finishes the run, and — on return — must cancel + join the poller.
	claim := api.ClaimResponse{Native: true, RunID: runID, JobName: "goleak-noop"}
	backend := newHostBackend(a, runID, claim.JobName, t.TempDir(), nil)
	RunClaim(ctx, a.Client, a.ID, claim, backend)

	// Explicit teardown BEFORE the leak check: close the server and release the
	// HTTP client's keep-alive connection goroutines so they aren't mistaken for
	// leaks. The cancel poller is then the only goroutine that could still be
	// alive — and only if RunClaim failed to join it.
	srv.Close()
	a.Client.http.CloseIdleConnections()

	goleak.VerifyNone(t, ignore)
}
