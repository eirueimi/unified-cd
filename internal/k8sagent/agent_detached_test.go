package k8sagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/stretchr/testify/assert"
)

// TestRun_DetachedClaimUsesSeparatePool verifies the k8s agent polls the detached
// pool (kind=detached) when MaxDetachedConcurrent > 0, from a semaphore separate
// from MaxConcurrent.
func TestRun_DetachedClaimUsesSeparatePool(t *testing.T) {
	const agentID = "k8s-detached-agent"
	var sawDetached, sawNormal atomic.Bool

	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("POST /api/v1/agents/register", ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/heartbeat", ok)
	mux.HandleFunc("DELETE /api/v1/agents/"+agentID, ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/reconcile", func(w http.ResponseWriter, _ *http.Request) {
		orchestrateWriteJSON(w, map[string]int{"failedRuns": 0})
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/claim", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "kind=detached") {
			sawDetached.Store(true)
		} else {
			sawNormal.Store(true)
		}
		orchestrateWriteJSON(w, api.ClaimResponse{}) // nothing to claim; keep looping
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := agentlib.NewClient(srv.URL, "tok")
	a := newK8sAgentForTest(t, Config{AgentID: agentID, Namespace: "ns", MaxConcurrent: 1, MaxDetachedConcurrent: 1}, client)

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)

	assert.True(t, sawNormal.Load(), "agent must still poll the normal pool")
	assert.True(t, sawDetached.Load(), "agent must poll the detached pool with kind=detached")
}

// TestRun_DetachedOffByDefault verifies detached claiming is off when
// MaxDetachedConcurrent is unset (0), preserving legacy behavior.
func TestRun_DetachedOffByDefault(t *testing.T) {
	const agentID = "k8s-nodetached-agent"
	var sawDetached atomic.Bool

	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("POST /api/v1/agents/register", ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/heartbeat", ok)
	mux.HandleFunc("DELETE /api/v1/agents/"+agentID, ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/reconcile", func(w http.ResponseWriter, _ *http.Request) {
		orchestrateWriteJSON(w, map[string]int{"failedRuns": 0})
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/claim", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "kind=detached") {
			sawDetached.Store(true)
		}
		orchestrateWriteJSON(w, api.ClaimResponse{})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := agentlib.NewClient(srv.URL, "tok")
	a := newK8sAgentForTest(t, Config{AgentID: agentID, Namespace: "ns", MaxConcurrent: 1}, client)

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)

	assert.False(t, sawDetached.Load(), "detached claiming must be off when MaxDetachedConcurrent is unset")
}
