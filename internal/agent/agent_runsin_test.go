package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
)

// TestExecuteRun_RunsInContainer_HostAgentHardError verifies the deterministic
// error branch in the host agent's step-dispatch switch (agent.go ~line 452):
// a step with runsIn.container on a plain host agent must hard-fail rather
// than silently falling back to executing the command on the host. This
// mirrors the runJobStages harness pattern from agent_finally_test.go
// (mock controller server driven via executeRun), extended to also capture
// ExitCode and to prove the step's Run command never executed on the host.
func TestExecuteRun_RunsInContainer_HostAgentHardError(t *testing.T) {
	const agentID = "runsin-agent"
	const runID = "run-runsin-container"

	var mu sync.Mutex
	var reportedStatus string
	var reportedExitCode int
	finishCh := make(chan string, 1)

	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, "marker.txt")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		if req.Status == "Running" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mu.Lock()
		reportedStatus = req.Status
		reportedExitCode = req.ExitCode
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(srv.Close)

	a := &Agent{
		ID:     agentID,
		Client: NewClient(srv.URL, "tok"),
	}

	// The Run command would create a marker file if it were ever executed on
	// the host. runsIn.container must short-circuit before that happens.
	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "test-runsin",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:      0,
				StageIndex: 0,
				Name:       "container-step",
				RunsIn:     &dsl.RunsIn{Container: "job"},
				Run:        "echo ran > " + shellQuote(markerPath),
			}},
		},
	}

	a.executeRun(context.Background(), claim, tmpDir)

	select {
	case s := <-finishCh:
		assert.Equal(t, "Failed", s, "run must not succeed when runsIn.container is used on the host agent")
	default:
		t.Fatal("FinishRun was not called")
	}

	mu.Lock()
	status := reportedStatus
	exitCode := reportedExitCode
	mu.Unlock()

	assert.Equal(t, "Failed", status, "step should be reported Failed, not silently run on host")
	assert.Equal(t, -1, exitCode, "host dispatch guard reports exit code -1 for the unsupported-container branch")

	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("marker file should not exist; runsIn.container step must not execute its Run command on the host agent (stat err: %v)", err)
	}
}

// shellQuote is a tiny helper to keep the marker-file path safe to embed in a
// shell command across the test's target platforms.
func shellQuote(s string) string {
	if !strings.ContainsAny(s, " \t") {
		return s
	}
	return "\"" + s + "\""
}

// TestExecuteRun_PodTemplate_NotRejectedOnHost verifies the host agent no
// longer hard-fails a claim merely because it carries a podTemplate: with the
// guard relaxed, an empty-stage podTemplate claim finishes Succeeded (the
// podTemplate is only consulted to resolve runsIn.container definitions).
func TestExecuteRun_PodTemplate_NotRejectedOnHost(t *testing.T) {
	const agentID = "pt-agent"
	const runID = "run-podtemplate"

	finishCh := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(srv.Close)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}
	claim := api.ClaimResponse{
		RunID:       runID,
		JobName:     "pt-job",
		PodTemplate: &dsl.PodTemplate{Spec: map[string]any{"containers": []any{}}},
		// No stages: nothing to run, so the claim should finish Succeeded.
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	select {
	case s := <-finishCh:
		assert.Equal(t, "Succeeded", s, "a podTemplate claim must no longer be rejected on the host agent")
	default:
		t.Fatal("FinishRun was not called")
	}
}
