package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/paritycases"
)

// parityHostHarness is a fake controller server mirroring the patterns used
// throughout agent_callrun_test.go / agent_stdout_stream_test.go /
// agent_if_test.go / agent_finally_test.go, generalized to serve ANY step
// index (the 10 parity cases use a handful of different stage layouts) via
// Go 1.22 {wildcard} mux patterns instead of one handler per fixed index.
type parityHostHarness struct {
	mu sync.Mutex

	// stepNameByIndex resolves a StepReportRequest.StepIndex back to the
	// step's display name, populated from every ReportStep body seen (a
	// step's very first report already carries its StepName).
	stepNameByIndex map[int]string

	// terminalStatus is keyed by "name" or "name@variant" (paritycases
	// VariantKey), holding only the LAST terminal (non-Running,
	// non-Skipped-is-terminal-too) status observed — Skipped is itself
	// terminal (no further reports follow for that step).
	terminalStatus map[string]string

	finishStatus string

	// logLines captures every shipped log line (both the single AppendLog
	// endpoint and the bulk endpoint), resolved to (stepName, stream, text).
	logLines []paritycases.LogLine

	// outputs captures SetStepOutputs bodies, keyed by step display name
	// (variant-qualified via DisplayName when MatrixKey is set — the query
	// param carries the raw variant key, but the step name path param plus
	// the fake's stepNameByIndex map already gives us the display name from
	// the ReportStep stream, so we key by the plain step name here since none
	// of the 10 cases needs per-variant outputs).
	outputs map[string]map[string]string

	// childRunID captures ChildRunID from a terminal StepReport, by step name.
	childRunID map[string]string

	secretsToServe map[string]string
	fetchedNames   []string
}

func newParityHostHarness() *parityHostHarness {
	return &parityHostHarness{
		stepNameByIndex: map[int]string{},
		terminalStatus:  map[string]string{},
		outputs:         map[string]map[string]string{},
		childRunID:      map[string]string{},
	}
}

// isTerminal reports whether status is a final per-step status (i.e. not an
// intermediate "Running" report).
func isTerminal(status string) bool {
	switch status {
	case "Succeeded", "Failed", "Skipped", "Cancelled":
		return true
	default:
		return false
	}
}

func (h *parityHostHarness) recordStepReport(req api.StepReportRequest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if req.StepName != "" {
		h.stepNameByIndex[req.StepIndex] = req.StepName
	}
	if isTerminal(req.Status) {
		// req.StepName is the DisplayName() ("build (a)" for a matrix
		// variant), not the plain step name; recover the plain name by
		// trimming the " (...)" suffix DisplayName appends when Variant != ""
		// so the VariantKey matches paritycases' "name@variant" convention.
		baseName := req.StepName
		if req.Variant != "" {
			if i := strings.Index(baseName, " ("); i >= 0 {
				baseName = baseName[:i]
			}
		}
		key := paritycases.VariantKey(baseName, req.Variant)
		h.terminalStatus[key] = req.Status
		if req.ChildRunID != "" {
			h.childRunID[baseName] = req.ChildRunID
		}
	}
}

func (h *parityHostHarness) recordLogLine(stepIndex int, stream, line string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	name := h.stepNameByIndex[stepIndex]
	h.logLines = append(h.logLines, paritycases.LogLine{Step: name, Stream: stream, Substring: line})
}

func (h *parityHostHarness) recordOutputs(stepIndex int, outputs map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	name := h.stepNameByIndex[stepIndex]
	if name == "" {
		name = fmt.Sprintf("step-%d", stepIndex)
	}
	h.outputs[name] = outputs
}

// newParityHostServer stands up an httptest.Server implementing every
// endpoint executeRun can call, generalized over step index/run id via
// {wildcard} path patterns (Go 1.22+ ServeMux).
func newParityHostServer(t *testing.T, agentID string, h *parityHostHarness) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.recordStepReport(req)
		w.WriteHeader(http.StatusNoContent)
	})
	// Single-line log endpoint (stdout is streamed via NewLogPusher too, but
	// some paths — e.g. AppendLog direct calls — use the non-bulk endpoint).
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, r *http.Request) {
		var req api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.recordLogLine(req.StepIndex, req.Stream, req.Line)
		w.WriteHeader(http.StatusNoContent)
	})
	// Bulk log endpoint: /api/v1/agents/{agentID}/runs/{runId}/steps/{idx}/logs/bulk
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		idx, _ := strconv.Atoi(r.PathValue("idx"))
		var reqs []api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&reqs)
		for _, l := range reqs {
			if l.Line != "" {
				h.recordLogLine(idx, l.Stream, l.Line)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/outputs", func(w http.ResponseWriter, r *http.Request) {
		idx, _ := strconv.Atoi(r.PathValue("idx"))
		var req api.SetOutputsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.recordOutputs(idx, req.Outputs)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.finishStatus = body.Status
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	// GetRun: used by the cancel-poller goroutine (never cancels in these
	// cases) — always report Running so the poller is a no-op.
	mux.HandleFunc("GET /api/v1/runs/{runId}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("runId")
		st := api.RunRunning
		if id == paritycases.ChildRunIDFixture {
			st = api.RunSucceeded
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Run{ID: id, Status: st})
	})
	mux.HandleFunc("GET /api/v1/runs/{runId}/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.RunOutputs{Outputs: map[string]string{}})
	})
	// CreateRun: for the call-succeeds-with-link case, always returns the
	// fixed child id.
	mux.HandleFunc("POST /api/v1/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Run{ID: paritycases.ChildRunIDFixture, Status: api.RunSucceeded})
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/secrets/fetch", func(w http.ResponseWriter, r *http.Request) {
		var req api.AgentFetchSecretsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.mu.Lock()
		h.fetchedNames = append(h.fetchedNames, req.Names...)
		toServe := h.secretsToServe
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.AgentFetchSecretsResponse{Secrets: toServe})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// observation converts the harness's recordings into a paritycases.Observation.
func (h *parityHostHarness) observation() paritycases.Observation {
	h.mu.Lock()
	defer h.mu.Unlock()
	statuses := make(map[string]string, len(h.terminalStatus))
	for k, v := range h.terminalStatus {
		statuses[k] = v
	}
	logs := append([]paritycases.LogLine(nil), h.logLines...)
	outputs := make(map[string]map[string]string, len(h.outputs))
	for k, v := range h.outputs {
		outputs[k] = v
	}
	childRunID := make(map[string]string, len(h.childRunID))
	for k, v := range h.childRunID {
		childRunID[k] = v
	}
	return paritycases.Observation{
		StepStatus:  statuses,
		RunFinished: h.finishStatus,
		Logs:        logs,
		Outputs:     outputs,
		ChildRunID:  childRunID,
	}
}

// TestParity_HostAgent drives every paritycases.Case through the real host
// agent's executeRun (real bash execution, no exec faking) and asserts the
// shared Expectation via paritycases.Assert.
func TestParity_HostAgent(t *testing.T) {
	for _, tc := range paritycases.Cases() {
		t.Run(tc.Name, func(t *testing.T) {
			runParityHostCase(t, tc)
		})
	}
}

func runParityHostCase(t *testing.T, tc paritycases.Case) {
	t.Helper()

	agentID := "parity-host-" + sanitizeName(tc.Name)
	h := newParityHostHarness()
	h.secretsToServe = tc.Secrets

	srv := newParityHostServer(t, agentID, h)

	a := &Agent{
		ID:     agentID,
		Client: NewClient(srv.URL, "tok"),
	}

	claim := tc.Claim()

	// post-hooks-lifo: the host agent's post: hook drain runs the script via
	// RunStepCapture with stdout/stderr never shipped to the log pipeline
	// (see paritycases.postHooksLIFO's doc comment), so this case observes
	// LIFO order out-of-band: each post script appends a line to a real file
	// via $POSTHOOK_MARKER_FILE (inherited from the test process env, since
	// RunStepCapture's cmd.Env = append(os.Environ(), extraEnv...)).
	var markerFile string
	if tc.Name == "post-hooks-lifo" {
		markerFile = filepath.Join(t.TempDir(), "posthook-order.txt")
		t.Setenv("POSTHOOK_MARKER_FILE", markerFile)
	}

	workDir := t.TempDir()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a.executeRun(ctx, claim, workDir)
	elapsed := time.Since(start)

	if tc.Name == "step-timeout-fails" {
		if elapsed >= 8*time.Second {
			t.Errorf("step-timeout-fails: expected executeRun to return well before the step's own sleep would (8s), took %s", elapsed)
		}
	}

	obs := h.observation()
	paritycases.Assert(t, tc.Expect, obs)

	if tc.Name == "call-succeeds-with-link" {
		if got := obs.ChildRunID["callChild"]; got != paritycases.ChildRunIDFixture {
			t.Errorf("call-succeeds-with-link: ChildRunID[%q] = %q, want %q", "callChild", got, paritycases.ChildRunIDFixture)
		}
	}

	if tc.Name == "post-hooks-lifo" {
		assertPostHookLIFOFromMarkerFile(t, markerFile)
	}
}

// assertPostHookLIFOFromMarkerFile reads the marker file each post: hook
// script appended a line to and asserts post-2 was written before post-1
// (LIFO: step2's post hook, appended to hookStack after step1's, drains
// first — see internal/agent/agent.go's hookStack `for i := len-1; i >= 0`
// drain loop).
func assertPostHookLIFOFromMarkerFile(t *testing.T, markerFile string) {
	t.Helper()
	data, err := os.ReadFile(markerFile)
	if err != nil {
		t.Fatalf("post-hooks-lifo: failed to read marker file %s: %v", markerFile, err)
	}
	lines := strings.Fields(strings.TrimSpace(string(data)))
	// Each line is like "post-2" / "post-1" per the post script's `echo post-N`.
	var order []string
	for _, l := range lines {
		if l == "post-1" || l == "post-2" {
			order = append(order, l)
		}
	}
	want := []string{"post-2", "post-1"}
	if len(order) != len(want) {
		t.Fatalf("post-hooks-lifo: marker file has %v, want exactly %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("post-hooks-lifo: marker order = %v, want %v (LIFO: step2's post before step1's)", order, want)
			break
		}
	}
}

// sanitizeName makes a case name safe to embed in an agent ID / URL path
// segment (the case names are already hyphenated lowercase, so this is a
// light touch, kept for safety against future case names with spaces).
func sanitizeName(name string) string {
	return strings.ReplaceAll(name, " ", "-")
}
