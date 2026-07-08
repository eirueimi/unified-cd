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

// TestExecuteRun_RunsInContainer_NoPodTemplate verifies the deterministic
// error branch for a NATIVE claim: a container: step must fail (exit -1),
// never silently running its command on the host, because a native claim has
// no claim pod to exec into. This mirrors the runJobStages harness pattern
// from agent_finally_test.go (mock controller server driven via executeRun),
// extended to also capture ExitCode and to prove the step's Run command never
// executed on the host. The claim is marked Native so executeRun does not try
// to build a claim pod (which needs a real container runtime the test lacks);
// the isolated container: path is covered by backend_isolated_test.go.
func TestExecuteRun_RunsInContainer_NoPodTemplate(t *testing.T) {
	const agentID = "runsin-agent"
	const runID = "run-runsin-nopt"

	var mu sync.Mutex
	var reportedStatus string
	var reportedExitCode int
	finishCh := make(chan string, 1)

	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, "marker.txt")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
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
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/logs/bulk", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
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
		RunID:   runID,
		JobName: "test-runsin",
		Native:  true,
		Stages: []api.ClaimStage{{Step: &api.ClaimStep{
			Index: 0, StageIndex: 0, Name: "container-step",
			Container: "tools",
			Run:       "echo ran > " + shellQuote(markerPath),
		}}},
	}

	a.executeRun(context.Background(), claim, tmpDir)

	select {
	case s := <-finishCh:
		assert.Equal(t, "Failed", s, "runsIn.container without a defining podTemplate must fail")
	default:
		t.Fatal("FinishRun was not called")
	}
	mu.Lock()
	status, exitCode := reportedStatus, reportedExitCode
	mu.Unlock()
	assert.Equal(t, "Failed", status)
	assert.Equal(t, -1, exitCode)
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("marker must not exist; step must not run on host (stat err: %v)", err)
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
// longer hard-fails a claim merely because it carries a podTemplate: an
// empty-stage claim finishes Succeeded. The claim is marked Native so the run
// needs no claim pod (and thus no real container runtime); the point under
// test is that a podTemplate on the claim is not itself a rejection reason.
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
		Native:      true,
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
