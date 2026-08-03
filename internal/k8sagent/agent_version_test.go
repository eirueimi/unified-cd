package k8sagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The k8s agent used to omit Version from its registration entirely, so
// GET /api/v1/agents showed an empty version for every k8s agent no matter
// how the image was built. It must report the same build variable the host
// agent reports.
func TestRun_RegistersWithBuildVersion(t *testing.T) {
	const agentID = "k8s-version-agent"
	old := agentlib.Version
	agentlib.Version = "v9.9.9"
	defer func() { agentlib.Version = old }()

	registered := make(chan api.AgentRegisterRequest, 1)
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		var req api.AgentRegisterRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		select {
		case registered <- req:
		default:
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/heartbeat", ok)
	mux.HandleFunc("DELETE /api/v1/agents/"+agentID, ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/reconcile", func(w http.ResponseWriter, _ *http.Request) {
		orchestrateWriteJSON(w, map[string]int{"failedRuns": 0})
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/claim", func(w http.ResponseWriter, _ *http.Request) {
		orchestrateWriteJSON(w, api.ClaimResponse{})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := agentlib.NewClient(srv.URL, "tok")
	a := newK8sAgentForTest(t, Config{AgentID: agentID, Namespace: "ns", MaxConcurrent: 1}, client)

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)

	select {
	case req := <-registered:
		assert.Equal(t, "v9.9.9", req.Version)
	default:
		t.Fatal("agent never registered")
	}
}
