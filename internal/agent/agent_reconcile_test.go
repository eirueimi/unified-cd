package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_ReconcilesOrphanedRunsBeforeClaiming pins the startup order that
// plugs the restart hole the stuck-run reaper cannot see: a restarted agent
// re-registers under the same ID and resumes heartbeating immediately, so
// runs its previous process claimed would stay Running forever. Run must call
// the reconcile endpoint after registering and BEFORE the first claim.
func TestRun_ReconcilesOrphanedRunsBeforeClaiming(t *testing.T) {
	const agentID = "reconcile-agent"

	var mu sync.Mutex
	var order []string
	record := func(what string) {
		mu.Lock()
		order = append(order, what)
		mu.Unlock()
	}
	claimed := make(chan struct{})
	var claimOnce sync.Once

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		record("register")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/reconcile", func(w http.ResponseWriter, r *http.Request) {
		record("reconcile")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"failedRuns":2}`)) //nolint:errcheck
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/claim", func(w http.ResponseWriter, r *http.Request) {
		record("claim")
		claimOnce.Do(func() { close(claimed) })
		w.WriteHeader(http.StatusNoContent) // nothing to claim
	})
	mux.HandleFunc("DELETE /api/v1/agents/"+agentID, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent) // shutdown deregister
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := NewWithLabels(agentID, nil, NewClient(srv.URL, "tok"))
	a.WorkspaceDir = t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = a.Run(ctx)
		close(done)
	}()

	select {
	case <-claimed:
	case <-time.After(10 * time.Second):
		t.Fatal("agent never reached the claim loop")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("agent did not shut down after cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, order)
	idx := map[string]int{}
	for i, what := range order {
		if _, seen := idx[what]; !seen {
			idx[what] = i
		}
	}
	reconcileIdx, ok := idx["reconcile"]
	require.True(t, ok, "Run must call the reconcile endpoint on startup; calls seen: %v", order)
	assert.Less(t, idx["register"], reconcileIdx, "reconcile must come after register")
	assert.Less(t, reconcileIdx, idx["claim"], "reconcile must complete before the first claim")
}
