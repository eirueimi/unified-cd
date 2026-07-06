package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runCallStepThroughFakeClient drives a run whose single step is
// `call: { job: <jobName> }` through the mock-HTTP-server harness (mirroring
// agent_finally_test.go / agent_if_test.go). The fake CreateRun returns a
// fixed child run ID; the fake child run reports Succeeded so the call
// completes. It returns every StepReportRequest observed for the call step,
// in arrival order.
func runCallStepThroughFakeClient(t *testing.T, jobName, childRunID string) []api.StepReportRequest {
	t.Helper()

	const agentID = "call-agent"
	const runID = "run-call"

	var mu sync.Mutex
	var reports []api.StepReportRequest

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		mu.Lock()
		reports = append(reports, req)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.Run{ID: childRunID, Status: api.RunSucceeded}) //nolint:errcheck
	})
	mux.HandleFunc("GET /api/v1/runs/"+childRunID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.Run{ID: childRunID, Status: api.RunSucceeded}) //nolint:errcheck
	})
	mux.HandleFunc("GET /api/v1/runs/"+childRunID+"/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.RunOutputs{Outputs: map[string]string{}}) //nolint:errcheck
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{
		ID:     agentID,
		Client: NewClient(srv.URL, "tok"),
	}

	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "test-call",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:      0,
				StageIndex: 0,
				Name:       "call-child",
				Call:       &api.ClaimCallStep{Job: jobName},
			}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	mu.Lock()
	defer mu.Unlock()
	return reports
}

// terminalCallReport returns the terminal (Succeeded/Failed) report, or nil.
func terminalCallReport(reports []api.StepReportRequest) *api.StepReportRequest {
	for i := range reports {
		if reports[i].Status == "Succeeded" || reports[i].Status == "Failed" {
			return &reports[i]
		}
	}
	return nil
}

func TestExecuteRun_CallStep_ReportsChildLink(t *testing.T) {
	// Drive a run whose single step is `call: { job: child-job }` through the
	// fake-client harness (mirror agent_finally_test.go). The fake CreateRun
	// returns a known child id; the fake child run reports Succeeded so the
	// call completes. Assert the terminal StepReport for the call step carries
	// ChildRunID == <that id> and CallJobName == "child-job".
	reports := runCallStepThroughFakeClient(t, "child-job", "fixed-child-run-id")
	rec := terminalCallReport(reports)
	require.NotNil(t, rec)
	assert.Equal(t, "fixed-child-run-id", rec.ChildRunID)
	assert.Equal(t, "child-job", rec.CallJobName)
}

// TestExecuteRun_CallStep_ReportsChildLinkWhileRunning pins that the child-run
// link is visible while the call step is still in flight, not only on the
// terminal report. Long-running child jobs are exactly when users need to
// navigate to the child's logs, so the agent must report ChildRunID on a
// non-terminal ("Running") report immediately after the child run is created.
func TestExecuteRun_CallStep_ReportsChildLinkWhileRunning(t *testing.T) {
	reports := runCallStepThroughFakeClient(t, "child-job", "fixed-child-run-id")

	var earlyLink *api.StepReportRequest
	for i := range reports {
		if reports[i].Status == "Running" && reports[i].ChildRunID != "" {
			earlyLink = &reports[i]
			break
		}
	}
	require.NotNil(t, earlyLink,
		"expected a non-terminal (Running) step report carrying ChildRunID; got reports: %+v", reports)
	assert.Equal(t, "fixed-child-run-id", earlyLink.ChildRunID)
	assert.Equal(t, "child-job", earlyLink.CallJobName)
	// The early link report must not carry a StartedAt: the controller maps a
	// zero StartedAt to NULL, and the UPSERT's COALESCE then preserves the
	// original started_at from the initial Running report.
	assert.True(t, earlyLink.StartedAt.IsZero(),
		"early link report must leave StartedAt zero so it cannot clobber the recorded start time")
}

// TestExecuteRun_CallStep_BadParamTemplate_FailsStep verifies that a call
// step whose `with:`/params template references a nonexistent field (e.g.
// "{{ .RunID }}", which is not a field of dsl.TemplateData) fails the step
// loudly instead of silently forwarding the raw unexpanded template string
// to the child job as an input. Critically, the child run must never be
// created at all (CreateRun must not be called) once param expansion has
// failed, since the would-be params are broken.
func TestExecuteRun_CallStep_BadParamTemplate_FailsStep(t *testing.T) {
	const agentID = "call-badparam-agent"
	const runID = "run-call-badparam"
	const childRunID = "should-never-exist"

	// Capture slog output so we can assert the error mentions the offending
	// param key, without adding any test-only hooks to production code.
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	var createRunCalled bool
	var mu sync.Mutex
	var finishStatus string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/runs", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		createRunCalled = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.Run{ID: childRunID, Status: api.RunSucceeded}) //nolint:errcheck
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		mu.Lock()
		finishStatus = body.Status
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{
		ID:     agentID,
		Client: NewClient(srv.URL, "tok"),
	}

	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "test-call-badparam",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:      0,
				StageIndex: 0,
				Name:       "call-child",
				Call: &api.ClaimCallStep{
					Job: "child-job",
					Params: map[string]string{
						"broken": "{{ .RunID }}",
					},
				},
			}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	mu.Lock()
	defer mu.Unlock()
	assert.False(t, createRunCalled, "CreateRun must not be called when a call param template fails to expand")
	assert.Equal(t, "Failed", finishStatus, "the run should fail when a call param template fails to expand")
	assert.Contains(t, logBuf.String(), "broken", "the log should mention the offending param key")
}
