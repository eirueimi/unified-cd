package k8sagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
)

// A heartbeat can recreate this agent's row without its capabilities, and the
// claim poll is what restores them, so every poll must carry the set even when
// the agent declares no volumes (the case claimRequest answers with nothing).
func TestRun_EveryClaimPollCarriesTheCapabilitySet(t *testing.T) {
	var mu sync.Mutex
	var claims []api.ClaimRequest
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, r *http.Request) { drainRequestBody(r); w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("POST /api/v1/agents/register", ok)
	mux.HandleFunc("POST /api/v1/agents/k8s-1/heartbeat", ok)
	mux.HandleFunc("DELETE /api/v1/agents/k8s-1", ok)
	mux.HandleFunc("POST /api/v1/agents/k8s-1/runs/reconcile", func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		orchestrateWriteJSON(w, map[string]int{"failedRuns": 0})
	})
	mux.HandleFunc("POST /api/v1/agents/k8s-1/claim", func(w http.ResponseWriter, r *http.Request) {
		// A detached claim sends no body by design; only the normal poll is
		// what restores the row.
		if r.URL.Query().Get("kind") != "detached" {
			var body api.ClaimRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			claims = append(claims, body)
			mu.Unlock()
		}
		orchestrateWriteJSON(w, api.ClaimResponse{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cs := fake.NewSimpleClientset()
	cfg := Config{AgentID: "k8s-1", AgentName: "agent1", Namespace: testVolNS, MaxConcurrent: 1}
	pm := NewPodManager(cs, testVolNS, "img")
	a := NewK8sAgent(cfg, agentlib.NewClient(srv.URL, "tok"), pm, nil, NewPodPool(cs, testVolNS, pm))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(claims) >= 2
	}, 5*time.Second, 20*time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	for i, c := range claims {
		assert.ElementsMatch(t, []string{"pod", "container"}, c.Capabilities, "claim poll %d", i)
	}
}
