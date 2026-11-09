package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
)

func TestFilterNative(t *testing.T) {
	caps := []string{"native", "container", "securitycontext"}
	assert.Equal(t, caps, (&Agent{}).filterNative(caps), "default keeps native")
	assert.Equal(t, []string{"container", "securitycontext"}, (&Agent{DisableNative: true}).filterNative(caps))
	assert.Empty(t, (&Agent{DisableNative: true}).filterNative([]string{"native"}))
}

func TestCapabilities_DisableNativeOmitsNative(t *testing.T) {
	// An unknown runtime preference makes runtime detection fail, leaving only
	// the native capability on a default agent.
	a := &Agent{RuntimePref: "no-such-runtime"}
	assert.Equal(t, []string{"native"}, a.capabilities())
	a = &Agent{RuntimePref: "no-such-runtime", DisableNative: true}
	assert.Empty(t, a.capabilities())
}

func TestExecuteRun_NativeClaimRefusedWhenNativeDisabled(t *testing.T) {
	const agentID = "dn-agent"
	const runID = "run-dn"
	var mu sync.Mutex
	var finished string
	var logs []string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/-1/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&reqs)
		mu.Lock()
		for _, l := range reqs {
			logs = append(logs, l.Line)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		finished = req.Status
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok"), DisableNative: true}
	claim := api.ClaimResponse{
		Native: true, RunID: runID, JobName: "j",
		Stages: []api.ClaimStage{{Step: &api.ClaimStep{Index: 0, Name: "s", Run: "echo hi"}}},
	}
	a.executeRun(context.Background(), claim, t.TempDir())

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, string(api.RunFailed), finished)
	require.NotEmpty(t, logs)
	assert.Contains(t, logs[0], "native execution is disabled")
}

func TestRun_RefusesToStartWithNoCapabilities(t *testing.T) {
	a := &Agent{ID: "x", RuntimePref: "no-such-runtime", DisableNative: true, Client: NewClient("http://127.0.0.1:1", "tok")}
	err := a.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no capabilities")
}
