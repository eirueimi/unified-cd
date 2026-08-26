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

// TestRetry_PerAttemptTimeoutThenSucceeds proves the per-attempt timeout
// bounds ONE attempt (not the whole retry budget): attempt 1 would run far
// longer than the timeout (sleep 30) and is killed ~9s in by its own
// timeoutMinutes-derived context, then attempt 2 (a trivial `true`) succeeds
// and the run ends Succeeded. The whole step therefore completes in well
// under the 30s attempt-1 sleep — if the timeout had been applied to the
// WHOLE step (the pre-Task-3 behavior) the retry budget would have been
// capped at ~9s and there would have been no room for attempt 2 to run at
// all.
//
// The per-attempt timeout (9s) is deliberately not razor-thin against the
// 30s sleep: on this backend both attempts go through RunStep, which really
// execs bash on the host per attempt (see retryHarness's doc comment). On
// Windows that bash is Git Bash, and spawning it (MSYS runtime init) is not
// free — measured cold-start-to-exit for a trivial `true` ranged ~0.7-1.2s
// on a quiet dev machine, so a ~1.2s timeout (this test's original value)
// left `true` racing its own attempt's deadline on a loaded CI box, and
// attempt 2 could be killed same as attempt 1, failing the test with only 1
// recorded attempt. 9s gives roughly an order of magnitude of headroom over
// that measured spawn cost. The elapsed assertion below is loosened to match
// (< 20s), which still leaves a comfortable factor below the 30s sleep to
// prove the timeout is per-attempt, not whole-step.
func TestRetry_PerAttemptTimeoutThenSucceeds(t *testing.T) {
	stubRetrySleep(t)

	const agentID = "retry-agent"
	const runID = "run-retry-attempt-timeout"

	h := newRetryHarness()
	srv := newRetryServer(t, agentID, h)
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	workDir := t.TempDir()
	counter := filepath.Join(workDir, "count.txt")
	// Attempt 1 (n==1): sleep 30 — far longer than the 9s per-attempt
	// timeout, so it is reliably killed by attempt 1's context. Attempt 2
	// (n>=2): `true` — exits 0 immediately, so the step succeeds on retry.
	script := "n=$(cat '" + counter + "' 2>/dev/null || echo 0); n=$((n+1)); printf '%s' \"$n\" > '" + counter + "';\n" +
		"if [ \"$n\" -eq 1 ]; then sleep 30; else true; fi\n"

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-retry-attempt-timeout",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0,
				Name:  "slow-then-fast",
				Run:   script,
				// 0.15 min = 9s: bounds attempt 1's sleep 30 with a huge
				// margin, but is per-ATTEMPT — it must not cap the retry
				// budget. See the test's doc comment for why this is 9s
				// rather than a razor-thin value: attempt 2's real work is a
				// fresh Git Bash process spawn on Windows, which alone can
				// take ~1s.
				TimeoutMinutes: 0.15,
				Retry:          &dsl.RetrySpec{Attempts: 2, Backoff: ""},
			}},
		},
	}

	start := time.Now()
	a.executeRun(context.Background(), claim, workDir)
	elapsed := time.Since(start)

	assert.Equal(t, "2", readCounter(t, counter), "expected exactly 2 attempts: slow attempt 1 killed, fast attempt 2 succeeds")
	assert.Equal(t, "Succeeded", h.lastStatusFor(0), "final reported status should be Succeeded after the retry")
	assert.Equal(t, "Succeeded", h.finishStatus)
	assert.Equal(t, 1, h.retrySeparatorCount(0), "expected one retry separator before attempt 2")
	// Well under the 30s attempt-1 sleep: proves attempt 1 was killed by its
	// per-attempt timeout AND that timeout did NOT cap the whole retry budget
	// (the run still reached and succeeded on attempt 2).
	require.Less(t, elapsed, 20*time.Second, "the whole step must finish well under attempt 1's 30s sleep (per-attempt timeout, not whole-step)")
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
// retried, even though more attempts remain. cancelledByMaster.Store(true)
// happens-before the poller cancels the run context (orchestrator.go's
// poller), so by the time the retry loop evaluates
// cancelledByMaster.Load() the flag is deterministically set — that part is
// not a timing race.
//
// What used to be a timing race: whether attempt 1's script had actually
// started (and written its counter file) before the fake GetRun endpoint
// started reporting Cancelled. An earlier version of this test set
// h.cancelled = true unconditionally before starting the run and relied on
// CancelPollInterval being comfortably larger than this host's real Git
// Bash process-spawn latency (RunStep execs a real bash.exe per attempt —
// see retryHarness's doc comment) — a fixed margin that measured ~400-460ms
// on a quiet Windows dev box but was observed to fail outright under
// concurrent CPU load (spawn latency exceeding the margin, so the attempt
// was cancelled before it ever wrote its counter, and the "exactly 1
// attempt ran" assertion below saw "0" instead of "1"). That is exactly the
// class of flake this suite otherwise guards against, just in a second
// test.
//
// The fix removes the margin instead of widening it: h.cancelled flips to
// true only once a background goroutine observes the counter file has
// actually been written, i.e. attempt 1 has demonstrably started. The
// poller can then never observe "cancelled" before that happens, regardless
// of how slow (or fast) process spawn is on the machine running the test.
// CancelPollInterval is shortened only for test speed now (it no longer has
// to outrun anything) — correctness no longer depends on its value.
func TestRetry_CancelNotRetried(t *testing.T) {
	stubRetrySleep(t)

	origPoll := CancelPollInterval
	CancelPollInterval = 100 * time.Millisecond
	t.Cleanup(func() { CancelPollInterval = origPoll })

	const agentID = "retry-agent"
	const runID = "run-retry-cancel"

	h := newRetryHarness()
	// h.cancelled starts false; the goroutine below (started after
	// a.executeRun begins) flips it once attempt 1 has genuinely started.
	srv := newRetryServer(t, agentID, h)
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	workDir := t.TempDir()
	counter := filepath.Join(workDir, "count.txt")
	// Sleep far longer than any plausible spawn+poll latency so the
	// poller's cancellation interrupts this attempt rather than it exiting
	// on its own. 120s (not 30s) deliberately leaves room for the elapsed
	// ceiling below to tolerate real OS scheduling delays under heavy
	// concurrent load — see that assertion's comment.
	script := "n=$(cat '" + counter + "' 2>/dev/null || echo 0); n=$((n+1)); printf '%s' \"$n\" > '" + counter + "'; sleep 120"

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

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if data, err := os.ReadFile(counter); err == nil && len(data) > 0 {
					h.mu.Lock()
					h.cancelled = true
					h.mu.Unlock()
					return
				}
			}
		}
	}()

	start := time.Now()
	a.executeRun(context.Background(), claim, workDir)
	elapsed := time.Since(start)

	// This ceiling exists only to prove the run was actually interrupted
	// rather than running the full 120s sleep to completion — it is not a
	// tight bound. In normal conditions the real cost is (spawn attempt 1) +
	// (one poll tick), both well under a second (see the many single-digit-
	// second runs typical of this test). But a direct A/B test on this
	// project's dev machine, run under concurrent CPU load from other
	// processes, saw one instance take 31s to return despite the
	// counter/status assertions below still holding (i.e. the interruption
	// itself, and this test's own goroutines, were delayed by the OS
	// scheduler, not broken) — so 60s, not something tighter like 25s, is
	// used here to stay clear of that kind of scheduling noise while still
	// leaving a comfortable 2x gap under the 120s sleep to catch a genuine
	// "cancellation stopped working" regression.
	require.Less(t, elapsed, 60*time.Second, "executeRun should return well before the full 120s sleep once cancelled")
	assert.Equal(t, "1", readCounter(t, counter), "the cancelled attempt must not be retried")
	assert.Equal(t, "Cancelled", h.lastStatusFor(0))
	assert.Equal(t, "Cancelled", h.finishStatus)
	assert.Equal(t, 0, h.retrySeparatorCount(0), "no retry separator should be logged for a cancelled attempt")
}
