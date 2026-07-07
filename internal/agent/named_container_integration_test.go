//go:build integration

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
)

// TestHostRunsInContainer_SharesWorkspace runs a real container via the
// detected runtime: a runsIn.container step writes into the workspace and a
// following host step reads it back, proving the bind mount shares the tree.
// Skips when no container runtime is available. This is the host-agent,
// named-container counterpart to TestExecuteRun_ScopedStep_RealRuntimeRoundTrip
// (agent_scope_integration_test.go), which round-trips through an isolated
// uses-scope container instead of a podTemplate-defined named container.
func TestHostRunsInContainer_SharesWorkspace(t *testing.T) {
	if _, err := crt.Detect(""); err != nil {
		t.Skipf("no container runtime available, skipping: %v", err)
	}

	const agentID = "named-container-integration-agent"
	const runID = "run-named-container-integration"

	var mu sync.Mutex
	stepLogs := map[int][]string{} // stepIndex -> stdout lines, in order
	finishCh := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	// One handler covers both steps' bulk-log endpoints (index varies in path).
	mux.HandleFunc("/api/v1/agents/"+agentID+"/runs/"+runID+"/steps/", func(w http.ResponseWriter, r *http.Request) {
		// Path shape: .../steps/{index}/logs/bulk
		var stepIndex int
		parts := strings.Split(r.URL.Path, "/")
		for i, p := range parts {
			if p == "steps" && i+1 < len(parts) {
				if idx, err := strconv.Atoi(parts[i+1]); err == nil {
					stepIndex = idx
				}
			}
		}
		var entries []api.LogAppendRequest
		if err := json.NewDecoder(r.Body).Decode(&entries); err == nil {
			mu.Lock()
			for _, e := range entries {
				if e.Stream == "stdout" {
					stepLogs[stepIndex] = append(stepLogs[stepIndex], e.Line)
				}
			}
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishCh <- body.Status:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := &Agent{
		ID:     agentID,
		Client: NewClient(srv.URL, "tok"),
	}

	const markerFile = "named-container-marker.txt"
	const markerContent = "hello from named container"

	resp := api.ClaimResponse{
		RunID:   runID,
		JobName: "test-named-container-integration",
		PodTemplate: &dsl.PodTemplate{Spec: map[string]any{
			"containers": []any{
				map[string]any{"name": "tools", "image": "alpine:3"},
			},
		}},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:      0,
				StageIndex: 0,
				Name:       "write-in-named-container",
				RunsIn:     &dsl.RunsIn{Container: "tools"},
				Run: "echo -n '" + markerContent + "' > " + markerFile +
					" && echo \"$UNIFIED_AGENT_OS\"",
			}},
			{Step: &api.ClaimStep{
				Index:      1,
				StageIndex: 1,
				Name:       "read-on-host",
				Run:        "cat " + markerFile,
			}},
		},
	}

	a.executeRun(context.Background(), resp, t.TempDir())

	select {
	case status := <-finishCh:
		assert.Equal(t, "Succeeded", status, "run should finish Succeeded")
	default:
		t.Fatal("FinishRun was not called")
	}

	mu.Lock()
	defer mu.Unlock()

	// Step 0 (the named-container step) must have reported UNIFIED_AGENT_OS=linux.
	containerStdout := strings.Join(stepLogs[0], "\n")
	assert.Contains(t, containerStdout, "linux",
		"runsIn.container step must observe UNIFIED_AGENT_OS=linux, got stdout: %q", containerStdout)

	// Step 1 (a plain host step) must be able to read the file the named
	// container wrote, proving the workspace bind mount is shared.
	hostStdout := strings.Join(stepLogs[1], "\n")
	assert.Contains(t, hostStdout, markerContent,
		"host step must read back the marker file written by the named container, got stdout: %q", hostStdout)
}
