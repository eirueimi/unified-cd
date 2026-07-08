package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// guardHarness is a minimal fake controller recording exactly what the
// orchestrator-level secret-outputs guard (RunClaim + FilterSecretOutputs,
// see internal/agent/orchestrator.go and outputsguard.go) is supposed to
// gate: which SetStepOutputs/SetRunOutputs bodies actually arrived, and which
// AppendLogBulk warning lines were shipped, per step index. Modeled after
// parityHostHarness / newParityHostServer in parity_host_test.go, trimmed
// down to only what these three scenarios need.
type guardHarness struct {
	mu sync.Mutex

	stepOutputsCalls []map[string]string // every SetStepOutputs body, in call order
	runOutputsCalls  []map[string]string // every SetRunOutputs body, in call order

	// logsByStep captures every AppendLogBulk line, keyed by (stepIndex, stream).
	logsByStep map[int][]api.LogAppendRequest

	secretsToServe map[string]string
	finishStatus   string
}

func newGuardHarness() *guardHarness {
	return &guardHarness{logsByStep: map[int][]api.LogAppendRequest{}}
}

func newGuardServer(t *testing.T, agentID string, h *guardHarness) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	// Bulk log endpoint used by warnSkippedOutput's AppendLogBulk call:
	// POST /api/v1/agents/{agentID}/runs/{runId}/steps/{idx}/logs/bulk
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		idx, _ := strconv.Atoi(r.PathValue("idx"))
		var reqs []api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&reqs)
		h.mu.Lock()
		h.logsByStep[idx] = append(h.logsByStep[idx], reqs...)
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/outputs", func(w http.ResponseWriter, r *http.Request) {
		var req api.SetOutputsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.mu.Lock()
		h.stepOutputsCalls = append(h.stepOutputsCalls, req.Outputs)
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/outputs", func(w http.ResponseWriter, r *http.Request) {
		var req api.SetOutputsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.mu.Lock()
		h.runOutputsCalls = append(h.runOutputsCalls, req.Outputs)
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
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
		h.mu.Lock()
		h.finishStatus = body.Status
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/secrets/fetch", func(w http.ResponseWriter, r *http.Request) {
		var req api.AgentFetchSecretsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.mu.Lock()
		toServe := h.secretsToServe
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.AgentFetchSecretsResponse{Secrets: toServe})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestRunClaim_StepOutputsGuard_SkipsSecretKeyKeepsCleanKey drives RunClaim
// end-to-end (real host backend, real bash execution, fake controller) with a
// step whose outputs map has one clean key and one secret-bearing key. It
// asserts: the secret key never reaches SetStepOutputs while the clean key
// does, and a warning line matching `output "<key>" skipped: value may
// contain a secret` is shipped via AppendLogBulk against the step's own index.
func TestRunClaim_StepOutputsGuard_SkipsSecretKeyKeepsCleanKey(t *testing.T) {
	const agentID = "guard-step-outputs-agent"
	const runID = "run-guard-step-outputs"
	const secretVal = "s3cr3t-token-value"

	h := newGuardHarness()
	h.secretsToServe = map[string]string{"MY_SECRET": secretVal}
	srv := newGuardServer(t, agentID, h)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	claim := api.ClaimResponse{
		Native:        true,
		RunID:         runID,
		JobName:       "test-step-outputs-guard",
		SecretsNeeded: []string{"MY_SECRET"},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0,
				Name:  "produce",
				Run:   "echo hi",
				Outputs: map[string]string{
					"clean": "hello",
					"leaky": "token={{ .Secrets.MY_SECRET }}",
				},
			}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	h.mu.Lock()
	defer h.mu.Unlock()

	require.Len(t, h.stepOutputsCalls, 1, "expected exactly one SetStepOutputs call")
	got := h.stepOutputsCalls[0]
	assert.Equal(t, "hello", got["clean"], "clean key should pass through")
	_, leakedPresent := got["leaky"]
	assert.False(t, leakedPresent, "secret-bearing key must never reach SetStepOutputs")

	lines := h.logsByStep[0]
	require.NotEmpty(t, lines, "expected a warning log line for step index 0")
	found := false
	for _, l := range lines {
		if l.Stream == "stderr" && l.StepIndex == 0 &&
			l.Line == fmt.Sprintf("output %q skipped: value may contain a secret", "leaky") {
			found = true
		}
	}
	assert.True(t, found, "expected a stderr line %q for step 0, got: %+v",
		fmt.Sprintf("output %q skipped: value may contain a secret", "leaky"), lines)

	assert.Equal(t, "Succeeded", h.finishStatus)
}

// TestRunClaim_StepOutputsGuard_AllSecretSkipsSetOutputsEntirely verifies that
// when EVERY key in a step's outputs is secret-bearing, SetStepOutputs is not
// called at all (FilterSecretOutputs returns an empty map and RunClaim's
// `if len(safe) > 0` guard skips the call), per the empty-after-filter case
// in the spec's test list.
func TestRunClaim_StepOutputsGuard_AllSecretSkipsSetOutputsEntirely(t *testing.T) {
	const agentID = "guard-step-outputs-allsecret-agent"
	const runID = "run-guard-step-outputs-allsecret"
	const secretVal = "s3cr3t-token-value"

	h := newGuardHarness()
	h.secretsToServe = map[string]string{"MY_SECRET": secretVal}
	srv := newGuardServer(t, agentID, h)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	claim := api.ClaimResponse{
		Native:        true,
		RunID:         runID,
		JobName:       "test-step-outputs-guard-allsecret",
		SecretsNeeded: []string{"MY_SECRET"},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0,
				Name:  "produce",
				Run:   "echo hi",
				Outputs: map[string]string{
					"leaky1": "token={{ .Secrets.MY_SECRET }}",
					"leaky2": "also={{ .Secrets.MY_SECRET }}",
				},
			}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	h.mu.Lock()
	defer h.mu.Unlock()

	assert.Empty(t, h.stepOutputsCalls, "SetStepOutputs must not be called when every output key is secret-bearing")

	lines := h.logsByStep[0]
	var skippedKeys []string
	for _, l := range lines {
		if l.Stream == "stderr" && l.StepIndex == 0 {
			skippedKeys = append(skippedKeys, l.Line)
		}
	}
	assert.Len(t, skippedKeys, 2, "expected a skip warning for each of the 2 secret-bearing keys, got: %+v", skippedKeys)

	assert.Equal(t, "Succeeded", h.finishStatus)
}

// TestRunClaim_RunOutputsGuard_SkipsSecretKeyWithSystemLog verifies the
// run-outputs promotion path (declared job outputs promoted from step
// outputs at claim end): a secret-bearing promoted value never reaches
// SetRunOutputs, and its warning is shipped with StepIndex -1 (rendered as
// "System" in the UI, since the pipeline has already ended and there is no
// step log writer at that point).
func TestRunClaim_RunOutputsGuard_SkipsSecretKeyWithSystemLog(t *testing.T) {
	const agentID = "guard-run-outputs-agent"
	const runID = "run-guard-run-outputs"
	const secretVal = "s3cr3t-token-value"

	h := newGuardHarness()
	h.secretsToServe = map[string]string{"MY_SECRET": secretVal}
	srv := newGuardServer(t, agentID, h)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	claim := api.ClaimResponse{
		Native:        true,
		RunID:         runID,
		JobName:       "test-run-outputs-guard",
		SecretsNeeded: []string{"MY_SECRET"},
		JobOutputs:    []string{"clean_out", "leaky_out"},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0,
				Name:  "produce",
				Run:   "echo hi",
				Outputs: map[string]string{
					"clean_out": "hello",
					"leaky_out": "token={{ .Secrets.MY_SECRET }}",
				},
			}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	h.mu.Lock()
	defer h.mu.Unlock()

	require.Len(t, h.runOutputsCalls, 1, "expected exactly one SetRunOutputs call")
	got := h.runOutputsCalls[0]
	assert.Equal(t, "hello", got["clean_out"], "clean promoted output should pass through")
	_, leakedPresent := got["leaky_out"]
	assert.False(t, leakedPresent, "secret-bearing promoted output must never reach SetRunOutputs")

	// warnSkippedOutput sends the run-outputs warning with stepIndex -1 (no
	// step log writer exists once the pipeline has finished).
	lines := h.logsByStep[-1]
	require.NotEmpty(t, lines, "expected a warning log line at stepIndex -1 for the run-outputs guard")
	found := false
	for _, l := range lines {
		if l.Stream == "stderr" && l.StepIndex == -1 &&
			l.Line == fmt.Sprintf("output %q skipped: value may contain a secret", "leaky_out") {
			found = true
		}
	}
	assert.True(t, found, "expected a stepIndex -1 stderr line %q, got: %+v",
		fmt.Sprintf("output %q skipped: value may contain a secret", "leaky_out"), lines)

	assert.Equal(t, "Succeeded", h.finishStatus)
}
