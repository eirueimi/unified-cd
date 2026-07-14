package agent

import (
	"context"
	"encoding/json"
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
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retryHarness is a minimal fake controller for driving RunClaim (via
// Agent.executeRun, real host backend, real bash execution — mirroring
// guardHarness in orchestrator_outputsguard_test.go) end-to-end for the
// step-retry loop in makeStepRunner (orchestrator.go). It records every
// ReportStep body (in call order, so the terminal status per step is the
// last entry with that StepIndex), every shipped log line (so retry
// separator lines can be asserted on/absent), and the final FinishRun
// status. cancelled, when true, makes the GetRun poll endpoint report the
// run as Cancelled from the very first poll.
type retryHarness struct {
	mu sync.Mutex

	reports      []api.StepReportRequest
	logsByStep   map[int][]api.LogAppendRequest
	finishStatus string

	cancelled bool
}

func newRetryHarness() *retryHarness {
	return &retryHarness{logsByStep: map[int][]api.LogAppendRequest{}}
}

func newRetryServer(t *testing.T, agentID string, h *retryHarness) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.mu.Lock()
		h.reports = append(h.reports, req)
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
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
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/v1/runs/{runId}", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		cancelled := h.cancelled
		h.mu.Unlock()
		status := api.RunRunning
		if cancelled {
			status = api.RunCancelled
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Run{ID: r.PathValue("runId"), Status: status})
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

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// lastStatusFor returns the status of the last ReportStep call recorded for
// stepIndex (the terminal report for that step).
func (h *retryHarness) lastStatusFor(stepIndex int) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	status := ""
	for _, r := range h.reports {
		if r.StepIndex == stepIndex {
			status = r.Status
		}
	}
	return status
}

// retrySeparatorCount counts how many "── retry" separator lines were shipped
// to stepIndex's stderr stream.
func (h *retryHarness) retrySeparatorCount(stepIndex int) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, l := range h.logsByStep[stepIndex] {
		if l.StepIndex == stepIndex && strings.Contains(l.Line, "── retry") {
			n++
		}
	}
	return n
}

// stubRetrySleep replaces the package-level retrySleep with a no-op for the
// duration of the calling test (t.Cleanup restores it), so tests configuring
// a real backoff duration still run instantly.
func stubRetrySleep(t *testing.T) {
	t.Helper()
	orig := retrySleep
	retrySleep = func(ctx context.Context, d time.Duration) error { return nil }
	t.Cleanup(func() { retrySleep = orig })
}

// countFileScript returns a bash script that increments a counter file
// (counting how many times this step body has actually executed) and then
// exits with exitFor(n) for the nth invocation (1-indexed).
func countFileScript(t *testing.T, counterPath string, exitFor func(n int) int) string {
	t.Helper()
	// The script itself only needs to increment + persist the counter and
	// exit with a code baked in per-line by the caller via a lookup table,
	// since bash has no access to the Go closure. Build an explicit
	// case/esac from exitFor for a small, known attempt ceiling (5 is more
	// than any test here needs).
	var b strings.Builder
	b.WriteString("n=$(cat '" + counterPath + "' 2>/dev/null || echo 0); n=$((n+1)); printf '%s' \"$n\" > '" + counterPath + "';\n")
	b.WriteString("case $n in\n")
	for n := 1; n <= 5; n++ {
		b.WriteString(strconv.Itoa(n) + ") exit " + strconv.Itoa(exitFor(n)) + " ;;\n")
	}
	b.WriteString("esac\n")
	return b.String()
}

func readCounter(t *testing.T, counterPath string) string {
	t.Helper()
	data, err := os.ReadFile(counterPath)
	if err != nil {
		return "0"
	}
	return string(data)
}

// TestRetry_FailsThenSucceeds: a step that fails twice then succeeds runs
// exactly 3 times (Attempts:3) and ends Succeeded.
func TestRetry_FailsThenSucceeds(t *testing.T) {
	stubRetrySleep(t)

	const agentID = "retry-agent"
	const runID = "run-retry-fts"

	h := newRetryHarness()
	srv := newRetryServer(t, agentID, h)
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	workDir := t.TempDir()
	counter := filepath.Join(workDir, "count.txt")
	script := countFileScript(t, counter, func(n int) int {
		if n < 3 {
			return 1
		}
		return 0
	})

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-retry-fts",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0,
				Name:  "flaky",
				Run:   script,
				Retry: &dsl.RetrySpec{Attempts: 3, Backoff: "1s"},
			}},
		},
	}

	a.executeRun(context.Background(), claim, workDir)

	assert.Equal(t, "3", readCounter(t, counter), "expected exactly 3 executions of the step body")
	assert.Equal(t, "Succeeded", h.lastStatusFor(0), "final reported status should be Succeeded")
	assert.Equal(t, "Succeeded", h.finishStatus)
	assert.Equal(t, 2, h.retrySeparatorCount(0), "expected a retry separator logged before attempts 2 and 3")
}

// TestRetry_AllFail: every attempt fails -> Failed, called exactly Attempts times.
func TestRetry_AllFail(t *testing.T) {
	stubRetrySleep(t)

	const agentID = "retry-agent"
	const runID = "run-retry-allfail"

	h := newRetryHarness()
	srv := newRetryServer(t, agentID, h)
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	workDir := t.TempDir()
	counter := filepath.Join(workDir, "count.txt")
	script := countFileScript(t, counter, func(n int) int { return 1 })

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-retry-allfail",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0,
				Name:  "always-fails",
				Run:   script,
				Retry: &dsl.RetrySpec{Attempts: 3, Backoff: "1s"},
			}},
		},
	}

	a.executeRun(context.Background(), claim, workDir)

	assert.Equal(t, "3", readCounter(t, counter), "expected exactly 3 executions of the step body")
	assert.Equal(t, "Failed", h.lastStatusFor(0))
	assert.Equal(t, "Failed", h.finishStatus)
	assert.Equal(t, 2, h.retrySeparatorCount(0))
}

// TestRetry_NoRetryRunsOnce: no retry: (or Attempts:1) runs exactly once.
func TestRetry_NoRetryRunsOnce(t *testing.T) {
	stubRetrySleep(t)

	const agentID = "retry-agent"
	const runID = "run-retry-once"

	h := newRetryHarness()
	srv := newRetryServer(t, agentID, h)
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	workDir := t.TempDir()
	counter := filepath.Join(workDir, "count.txt")
	script := countFileScript(t, counter, func(n int) int { return 1 })

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-retry-once",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0,
				Name:  "no-retry",
				Run:   script,
				// Retry left nil: default attempts = 1.
			}},
		},
	}

	a.executeRun(context.Background(), claim, workDir)

	assert.Equal(t, "1", readCounter(t, counter), "expected exactly 1 execution of the step body")
	assert.Equal(t, "Failed", h.lastStatusFor(0))
	assert.Equal(t, "Failed", h.finishStatus)
	assert.Equal(t, 0, h.retrySeparatorCount(0))
}

// TestRetry_CancelNotRetried: a master cancellation arriving while attempt 1
// is running must stop the loop immediately — the cancellation is never
// retried, even though more attempts remain. The step body sleeps well
// beyond the (shortened) cancel-poll interval so it is guaranteed to be
// killed by the run's context cancellation rather than exiting on its own;
// cancelledByMaster.Store(true) happens-before the poller cancels the run
// context (orchestrator.go's poller), so by the time the retry loop
// evaluates cancelledByMaster.Load() the flag is deterministically set — this
// is not a timing race for the loop's own branch, only the wall-clock speed
// of the test depends on scheduling.
func TestRetry_CancelNotRetried(t *testing.T) {
	stubRetrySleep(t)

	// The poll interval must comfortably exceed this environment's git-bash
	// process-spawn overhead (observed ~400-460ms for a trivial script on
	// this Windows host) so the step's first attempt is guaranteed to have
	// started — and written its counter file — before the poller's first
	// tick lands the cancellation. 1500ms leaves a large margin over that
	// while still keeping the test well under the default 5s interval.
	origPoll := CancelPollInterval
	CancelPollInterval = 1500 * time.Millisecond
	t.Cleanup(func() { CancelPollInterval = origPoll })

	const agentID = "retry-agent"
	const runID = "run-retry-cancel"

	h := newRetryHarness()
	h.cancelled = true // GetRun reports Cancelled from the very first poll
	srv := newRetryServer(t, agentID, h)
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	workDir := t.TempDir()
	counter := filepath.Join(workDir, "count.txt")
	// Sleep far longer than CancelPollInterval so the poller's cancellation
	// interrupts this attempt rather than it exiting on its own.
	script := "n=$(cat '" + counter + "' 2>/dev/null || echo 0); n=$((n+1)); printf '%s' \"$n\" > '" + counter + "'; sleep 30"

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-retry-cancel",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0,
				Name:  "cancel-me",
				Run:   script,
				Retry: &dsl.RetrySpec{Attempts: 5, Backoff: "1ms"},
			}},
		},
	}

	start := time.Now()
	a.executeRun(context.Background(), claim, workDir)
	elapsed := time.Since(start)

	require.Less(t, elapsed, 15*time.Second, "executeRun should return promptly once cancelled (sleep 30 interrupted)")
	assert.Equal(t, "1", readCounter(t, counter), "the cancelled attempt must not be retried")
	assert.Equal(t, "Cancelled", h.lastStatusFor(0))
	assert.Equal(t, "Cancelled", h.finishStatus)
	assert.Equal(t, 0, h.retrySeparatorCount(0), "no retry separator should be logged for a cancelled attempt")
}
