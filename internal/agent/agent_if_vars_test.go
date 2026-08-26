package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ifVarsHarness spins up the minimal controller surface executeRun talks to and
// records step statuses plus the run's OWN log lines (stepIndex -1, "System" in
// the UI), which is where an if:-condition diagnostic has to land.
type ifVarsHarness struct {
	mu          sync.Mutex
	stepReports map[int][]string
	systemLines []string
	finish      chan string
}

func newIfVarsHarness(t *testing.T, runID string) (*ifVarsHarness, *Agent) {
	t.Helper()
	h := &ifVarsHarness{
		stepReports: map[int][]string{},
		finish:      make(chan string, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		h.mu.Lock()
		h.stepReports[req.StepIndex] = append(h.stepReports[req.StepIndex], req.Status)
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case h.finish <- body["status"]:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/"+runID+"/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("idx") == "-1" {
			var lines []api.LogAppendRequest
			json.NewDecoder(r.Body).Decode(&lines) //nolint:errcheck
			h.mu.Lock()
			for _, l := range lines {
				h.systemLines = append(h.systemLines, l.Line)
			}
			h.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/"+runID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.Run{ID: runID, Status: api.RunRunning}) //nolint:errcheck
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, &Agent{ID: "a1", Client: NewClient(srv.URL, "tok")}
}

func (h *ifVarsHarness) reports(idx int) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.stepReports[idx]...)
}

func (h *ifVarsHarness) systemLog() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.systemLines, "\n")
}

// A vars-gated if: must actually GATE — the whole point of binding vars into
// the CEL environment. Before that, the expression did not compile, the error
// path ran the step (fail-safe), and nothing in the run said why.
func TestExecuteRun_IfVarsGates(t *testing.T) {
	h, a := newIfVarsHarness(t, "run-vars-gate")

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   "run-vars-gate",
		JobName: "test",
		Vars:    map[string]string{"ENV": "staging"},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Name: "prod-only", Index: 0, If: `vars.ENV == "prod"`, Run: "echo should-not-run"}},
			{Step: &api.ClaimStep{Name: "staging-only", Index: 1, If: `vars.ENV == "staging"`, Run: "echo ran"}},
		},
	}
	a.executeRun(context.Background(), claim, t.TempDir())

	assert.Equal(t, []string{"Skipped"}, h.reports(0), "a vars gate that does not match must skip the step")
	assert.Contains(t, h.reports(1), "Succeeded", "a vars gate that matches must run the step")
	assert.Empty(t, h.systemLog(), "a well-formed vars gate must not produce diagnostics")
}

// An UNDEFINED vars key reads as empty rather than erroring, so the gate stays
// shut instead of failing open — and the run's own log says which key it was.
func TestExecuteRun_IfVarsUndefinedKey_SkipsAndRecordsSystemLine(t *testing.T) {
	h, a := newIfVarsHarness(t, "run-vars-typo")

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   "run-vars-typo",
		JobName: "test",
		Vars:    map[string]string{"ENV": "prod"},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Name: "typo", Index: 0, If: `vars.EVN == "prod"`, Run: "echo should-not-run"}},
		},
	}
	a.executeRun(context.Background(), claim, t.TempDir())

	assert.Equal(t, []string{"Skipped"}, h.reports(0), "an undefined vars key must not fail open")
	log := h.systemLog()
	require.NotEmpty(t, log, "an undefined vars key must not be silent")
	assert.Contains(t, log, "vars.EVN")
	assert.Contains(t, log, `step "typo"`)
}

// A condition that cannot be evaluated still runs the step (fail-safe), but
// that decision now leaves a trace in the run's own log instead of only in the
// agent's slog on some other host.
func TestExecuteRun_IfCompileError_RunsAndRecordsSystemLine(t *testing.T) {
	h, a := newIfVarsHarness(t, "run-if-broken")

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   "run-if-broken",
		JobName: "test",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Name: "broken", Index: 0, If: `params.env ==`, Run: "echo ran"}},
		},
	}
	a.executeRun(context.Background(), claim, t.TempDir())

	assert.Contains(t, h.reports(0), "Succeeded", "an uncompilable condition is fail-safe: the step runs")
	log := h.systemLog()
	require.NotEmpty(t, log, "a fail-open condition must not be silent in the run")
	assert.Contains(t, log, "fail-safe")
	assert.Contains(t, log, `step "broken"`)
}
