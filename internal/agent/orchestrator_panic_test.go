package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probeBackend is a minimal ExecBackend whose RunDefault panics for a step
// named "boom" (and succeeds for any other step), used to drive the REAL
// RunClaim orchestration loop and prove that a panic in a step's backend exec
// is handled exactly like a normal step failure: the step's terminal
// ReportStep is Failed (NOT left "Running"), the panic text reaches the step's
// own log, and the failure flows through the existing machinery
// (continueOnError suppression, subsequent-step auto-skip, overall run status)
// — without crashing the process. Every other method is an inert stub (no step
// in these tests reaches them).
type probeBackend struct{}

func (probeBackend) RunDefault(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error) {
	if step.Name == "boom" {
		panic("kaboom in RunDefault")
	}
	return 0, nil
}

func (probeBackend) RunNamedContainer(ctx context.Context, step api.ClaimStep, container, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return 0, nil
}

func (probeBackend) EnsureScope(ctx context.Context, step api.ClaimStep, env []string) (ScopeHandle, error) {
	return ScopeHandle{}, nil
}

func (probeBackend) RunInScope(ctx context.Context, h ScopeHandle, script string, shell []string, env []string, stdout, stderr io.Writer) (int, error) {
	return 0, nil
}
func (probeBackend) CloseScopes(ctx context.Context) {}

func (probeBackend) CacheRestore(ctx context.Context, scope ScopeHandle, key string, restoreKeys []string, path string) (bool, error) {
	return false, nil
}

func (probeBackend) CacheSave(ctx context.Context, scope ScopeHandle, key, path string, ttlDays int) error {
	return nil
}

func (probeBackend) UploadArtifact(ctx context.Context, scope ScopeHandle, runID, name, path string) error {
	return nil
}

func (probeBackend) DownloadArtifact(ctx context.Context, scope ScopeHandle, runID, name, destDir string) error {
	return nil
}

func (probeBackend) RunPostHook(ctx context.Context, scope ScopeHandle, container, script string, shell []string, env []string, stdout, stderr io.Writer) error {
	return nil
}

func (probeBackend) ResolveArtifactPath(scope ScopeHandle, p string) (string, error) { return p, nil }
func (probeBackend) ResolveCachePath(scope ScopeHandle, p string) (string, error)    { return p, nil }
func (probeBackend) WorkspacePath(scope ScopeHandle) string                          { return "/workspace" }
func (probeBackend) DefaultAgentOS() string                                          { return "linux" }
func (probeBackend) SetMasker(m *secrets.Masker)                                     {}

func (probeBackend) StepLogWriters(ctx context.Context, stepIndex int) (io.Writer, io.Writer, func(ctx context.Context)) {
	return io.Discard, io.Discard, func(context.Context) {}
}
func (probeBackend) ConcurrencyMode() ConcurrencyMode { return Concurrent }

// panicProbeRecorder captures what the fake controller observed while driving
// RunClaim with probeBackend: the ordered per-step ReportStep statuses, all
// shipped log lines, and the final FinishRun status.
type panicProbeRecorder struct {
	mu           sync.Mutex
	stepStatuses map[int][]string
	logLines     []string
	finishCh     chan string
}

// terminalStatus returns the LAST status reported for stepIndex (its terminal
// state), and whether any status was reported at all.
func (rec *panicProbeRecorder) terminalStatus(stepIndex int) (string, bool) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	s := rec.stepStatuses[stepIndex]
	if len(s) == 0 {
		return "", false
	}
	return s[len(s)-1], true
}

func (rec *panicProbeRecorder) statuses(stepIndex int) []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.stepStatuses[stepIndex]...)
}

func (rec *panicProbeRecorder) joinedLogs() string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return strings.Join(rec.logLines, "\n")
}

// newPanicProbeServer builds a fake controller recording ReportStep statuses
// per step, log-bulk lines (any step index), and FinishRun status. The cancel
// poller's GetRun always reports Running so it never self-cancels the run.
func newPanicProbeServer(t *testing.T, agentID, runID string) (*httptest.Server, *panicProbeRecorder) {
	t.Helper()
	rec := &panicProbeRecorder{
		stepStatuses: map[int][]string{},
		finishCh:     make(chan string, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	// Poller: report Running so it never self-cancels the run.
	mux.HandleFunc("GET /api/v1/runs/"+runID, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Run{ID: runID, Status: api.RunRunning}) //nolint:errcheck
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var body api.StepReportRequest
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		rec.mu.Lock()
		rec.stepStatuses[body.StepIndex] = append(rec.stepStatuses[body.StepIndex], body.Status)
		rec.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	// Log-bulk for any step index (the panic guard ships to the panicking step's index).
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var lines []api.LogAppendRequest
		json.NewDecoder(r.Body).Decode(&lines) //nolint:errcheck
		rec.mu.Lock()
		for _, l := range lines {
			rec.logLines = append(rec.logLines, l.Line)
		}
		rec.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case rec.finishCh <- body.Status:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, rec
}

// TestRunClaim_StepPanic_ReportsFailedNotRunning is the key regression test for
// the review finding: a panic in a step's backend exec must fail the step (and
// run) with a terminal Failed ReportStep and the panic text in the step's log,
// rather than leaving the step stuck "Running" (which happened when runOne's
// recover was the only guard — it turned the panic into a returned error that
// marked the RUN Failed but skipped the closure's terminal step report + log).
func TestRunClaim_StepPanic_ReportsFailedNotRunning(t *testing.T) {
	const agentID = "panic-orch-agent"
	const runID = "run-orch-panic"

	srv, rec := newPanicProbeServer(t, agentID, runID)
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-orch-panic",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "echo hi"}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	assert.NotPanics(t, func() {
		RunClaim(ctx, a.Client, a.ID, claim, probeBackend{})
	}, "a panicking step must not crash RunClaim / the process")

	select {
	case status := <-rec.finishCh:
		assert.Equal(t, "Failed", status, "a panicking step should finish the run Failed")
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun was not called after the step panic")
	}

	statuses := rec.statuses(0)
	require.NotEmpty(t, statuses, "the panicking step must have reported at least one status")
	// The step must reach a terminal Failed report, not be left stuck at "Running".
	assert.Equal(t, "Failed", statuses[len(statuses)-1], "the step's terminal ReportStep must be Failed, not left Running (got sequence %v)", statuses)

	// The panic text must reach the step's own author-visible log.
	joined := rec.joinedLogs()
	assert.Contains(t, joined, "step panicked", "the step log must carry the panic marker")
	assert.Contains(t, joined, "kaboom in RunDefault", "the step log must carry the panic value")
}

// TestRunClaim_ContinueOnErrorPanic_RunNotFailed locks in that a panic on a
// continueOnError step is suppressed exactly like a normal error on such a
// step: the step itself is reported Failed, but the run is NOT failed and
// subsequent steps run normally.
func TestRunClaim_ContinueOnErrorPanic_RunNotFailed(t *testing.T) {
	const agentID = "panic-coe-agent"
	const runID = "run-coe-panic"

	srv, rec := newPanicProbeServer(t, agentID, runID)
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-coe-panic",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "echo hi", ContinueOnError: true}},
			{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "ok", Run: "echo ok"}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	assert.NotPanics(t, func() {
		RunClaim(ctx, a.Client, a.ID, claim, probeBackend{})
	}, "a continueOnError panic must not crash RunClaim")

	select {
	case status := <-rec.finishCh:
		assert.Equal(t, "Succeeded", status, "a continueOnError panic must not fail the whole run")
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun was not called")
	}

	term0, ok0 := rec.terminalStatus(0)
	require.True(t, ok0, "the panicking continueOnError step must have reported a status")
	assert.Equal(t, "Failed", term0, "the continueOnError step's terminal ReportStep must still be Failed (got %v)", rec.statuses(0))

	term1, ok1 := rec.terminalStatus(1)
	require.True(t, ok1, "the subsequent step must have run and reported a status")
	assert.Equal(t, "Succeeded", term1, "the step after a continueOnError panic must run and Succeed (got %v)", rec.statuses(1))
}

// TestRunClaim_Panic_SkipsSubsequentSteps locks in that a panic on a normal
// (non-continueOnError) step fails the run and causes subsequent steps to
// auto-skip, exactly as a normal step failure does.
func TestRunClaim_Panic_SkipsSubsequentSteps(t *testing.T) {
	const agentID = "panic-skip-agent"
	const runID = "run-skip-panic"

	srv, rec := newPanicProbeServer(t, agentID, runID)
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-skip-panic",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "echo hi"}},
			{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "next", Run: "echo next"}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	assert.NotPanics(t, func() {
		RunClaim(ctx, a.Client, a.ID, claim, probeBackend{})
	}, "a panicking step must not crash RunClaim")

	select {
	case status := <-rec.finishCh:
		assert.Equal(t, "Failed", status, "a non-continueOnError panic must fail the run")
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun was not called")
	}

	term0, ok0 := rec.terminalStatus(0)
	require.True(t, ok0, "the panicking step must have reported a status")
	assert.Equal(t, "Failed", term0, "the panicking step's terminal ReportStep must be Failed (got %v)", rec.statuses(0))

	term1, ok1 := rec.terminalStatus(1)
	require.True(t, ok1, "the subsequent step must have reported a (Skipped) status")
	assert.Equal(t, "Skipped", term1, "the step after a non-continueOnError panic must be Skipped (got %v)", rec.statuses(1))
}
