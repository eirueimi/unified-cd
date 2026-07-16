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

// probeBackend is a minimal ExecBackend whose RunDefault panics, used to drive
// the REAL RunClaim orchestration loop and prove that a panic in a step's
// backend exec is handled exactly like a normal step failure: the step's
// terminal ReportStep is Failed (NOT left "Running"), the panic text reaches
// the step's own log, and the run finishes Failed — without crashing the
// process. Every other method is an inert stub (no step in this test reaches
// them).
type probeBackend struct{}

func (probeBackend) RunDefault(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error) {
	panic("kaboom in RunDefault")
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

// TestRunClaim_StepPanic_ReportsFailedNotRunning is the key regression test for
// the review finding: a panic in a step's backend exec must fail the step (and
// run) with a terminal Failed ReportStep and the panic text in the step's log,
// rather than leaving the step stuck "Running" (which happened when runOne's
// recover was the only guard — it turned the panic into a returned error that
// marked the RUN Failed but skipped the closure's terminal step report + log).
func TestRunClaim_StepPanic_ReportsFailedNotRunning(t *testing.T) {
	const agentID = "panic-orch-agent"
	const runID = "run-orch-panic"

	var mu sync.Mutex
	stepStatuses := map[int][]string{} // stepIndex -> ordered list of reported statuses
	var logLines []string
	finishCh := make(chan string, 1)

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
		mu.Lock()
		stepStatuses[body.StepIndex] = append(stepStatuses[body.StepIndex], body.Status)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	// The panic guard ships the panic text via AppendLogBulk to the step's index.
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var lines []api.LogAppendRequest
		json.NewDecoder(r.Body).Decode(&lines) //nolint:errcheck
		mu.Lock()
		for _, l := range lines {
			logLines = append(logLines, l.Line)
		}
		mu.Unlock()
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
	case status := <-finishCh:
		assert.Equal(t, "Failed", status, "a panicking step should finish the run Failed")
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun was not called after the step panic")
	}

	mu.Lock()
	defer mu.Unlock()

	statuses := stepStatuses[0]
	require.NotEmpty(t, statuses, "the panicking step must have reported at least one status")
	// The step must reach a terminal Failed report, not be left stuck at "Running".
	assert.Equal(t, "Failed", statuses[len(statuses)-1], "the step's terminal ReportStep must be Failed, not left Running (got sequence %v)", statuses)

	// The panic text must reach the step's own author-visible log.
	joined := strings.Join(logLines, "\n")
	assert.Contains(t, joined, "step panicked", "the step log must carry the panic marker")
	assert.Contains(t, joined, "kaboom in RunDefault", "the step log must carry the panic value")
}
