//go:build k8s

package k8sagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestK8sAgent_ExecuteRun_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, restCfg := newTestKubeClient(t)
	ns := newTestNamespace(t, client)

	const agentID = "k8s-e2e"
	const runID = "run-e2e"

	var mu sync.Mutex
	var logLines []string
	finishCh := make(chan api.RunStatus, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	// stdout lines from logLineWriter
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, r *http.Request) {
		var req api.LogAppendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			mu.Lock()
			logLines = append(logLines, req.Line)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// stderr bulk from LogPusher.Flush
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		if err := json.NewDecoder(r.Body).Decode(&reqs); err == nil {
			mu.Lock()
			for _, req := range reqs {
				if req.Line != "" {
					logLines = append(logLines, req.Line)
				}
			}
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishCh <- api.RunStatus(body["status"]):
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pm := NewPodManager(client, ns, testImage)
	exec := NewExecutor(client, restCfg, ns)
	pool := NewPodPool(client, ns, pm)
	agentClient := agentlib.NewClient(srv.URL, "tok")

	cfg := Config{
		AgentID:   agentID,
		Namespace: ns,
		PodImage:  testImage,
	}
	a := NewK8sAgent(cfg, agentClient, pm, exec, pool)

	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "e2e-test",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, Name: "hello", Run: "echo hello-from-k8s-agent"}},
		},
	}

	a.executeRun(ctx, claim)

	select {
	case status := <-finishCh:
		require.Equal(t, api.RunSucceeded, status, "run should succeed")
	case <-time.After(30 * time.Second):
		t.Fatal("FinishRun not called within 30 seconds")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.NotEmpty(t, logLines, "expected at least one stdout log line")
	found := false
	for _, line := range logLines {
		if strings.Contains(line, "hello-from-k8s-agent") {
			found = true
			break
		}
	}
	assert.True(t, found, "expected log line containing 'hello-from-k8s-agent', got: %v", logLines)
}

func TestK8sAgent_ExecuteRun_StepFailure_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, restCfg := newTestKubeClient(t)
	ns := newTestNamespace(t, client)

	const agentID = "k8s-e2e-fail"
	const runID = "run-e2e-fail"

	finishCh := make(chan api.RunStatus, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishCh <- api.RunStatus(body["status"]):
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pm := NewPodManager(client, ns, testImage)
	exec := NewExecutor(client, restCfg, ns)
	pool := NewPodPool(client, ns, pm)
	agentClient := agentlib.NewClient(srv.URL, "tok")

	cfg := Config{AgentID: agentID, Namespace: ns, PodImage: testImage}
	a := NewK8sAgent(cfg, agentClient, pm, exec, pool)

	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "e2e-fail-test",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, Name: "fail-step", Run: "exit 42"}},
		},
	}

	a.executeRun(ctx, claim)

	select {
	case status := <-finishCh:
		assert.Equal(t, api.RunFailed, status, "run should fail when step exits non-zero")
	case <-time.After(30 * time.Second):
		t.Fatal("FinishRun not called within 30 seconds")
	}
}
