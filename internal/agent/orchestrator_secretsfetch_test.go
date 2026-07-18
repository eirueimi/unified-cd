package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
)

// TestRunClaim_FetchSecretsFails_FailsRunWithoutRunningSteps is the agent-side
// regression test for the fail-fast branch in RunClaim (orchestrator.go,
// ~line 161): when client.FetchSecrets returns an error, RunClaim must not
// silently continue with an empty secrets map (the old behavior — see
// c2f9528's removed `slog.Warn(...); secretValues = map[string]string{}`).
// Instead it must log the reason, FinishRun(..., api.RunFailed), and return
// BEFORE the step DAG runs at all.
//
// Modeled on guardHarness/newGuardServer (orchestrator_outputsguard_test.go)
// and reapServer (orchestrator_reap_test.go), trimmed to this scenario: the
// secrets/fetch route is made to fail (500), and a ReportStep counter proves
// the DAG never starts — ReportStep is called at the top of every step
// runner (see makeStepRunner / executeUploadArtifact in orchestrator.go)
// before any step body executes, so zero calls is a direct proxy for "no
// step ran".
func TestRunClaim_FetchSecretsFails_FailsRunWithoutRunningSteps(t *testing.T) {
	const agentID = "secretsfetch-fail-agent"
	const runID = "run-secretsfetch-fail"

	var reportStepCalls atomic.Int32
	var finishStatus atomic.Value
	finishStatus.Store("")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	// The step-report endpoint: hit once per step at the start of its runner.
	// A run whose secrets fetch fails must never hit this.
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		reportStepCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	// Simulates a missing/undecryptable secret: the controller endpoint
	// FetchSecrets calls returns an error (any 4xx/5xx surfaces as a non-nil
	// *HTTPError from Client.do).
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/secrets/fetch", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret not found", http.StatusNotFound)
	})
	mux.HandleFunc("GET /api/v1/runs/{runId}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Run{ID: r.PathValue("runId"), Status: api.RunRunning})
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		finishStatus.Store(body.Status)
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	claim := api.ClaimResponse{
		Native:        true,
		RunID:         runID,
		JobName:       "test-secretsfetch-fail",
		SecretsNeeded: []string{"MY_SECRET"},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, Name: "should-not-run", Run: "echo should-not-run"}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	assert.Equal(t, string(api.RunFailed), finishStatus.Load(),
		"a fetch-secrets failure must finish the run as Failed")
	assert.Equal(t, int32(0), reportStepCalls.Load(),
		"the step DAG must never run when secrets fetch fails")
}
