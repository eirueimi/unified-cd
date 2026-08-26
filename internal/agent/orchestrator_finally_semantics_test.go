package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// This file is the regression suite for four defects that shared one cause:
// anything implemented or validated only on the MAIN DAG path was accepted by
// the parser and then silently ignored during the `finally:` phase.
//
//	F4 — nothing bounded the phase at all. context.WithoutCancel strips the
//	     job-level deadline along with the cancellation and yields a context
//	     whose Done() is nil, so a `call:`/`run:` finally step with no per-step
//	     timeoutMinutes waited forever. Same for the two hook drains.
//	F3 — a failing finally step on a CANCELLED run reported Cancelled and left
//	     the run Cancelled, contradicting dsl.Spec.Finally's own documented
//	     contract ("A finally step that fails marks the run Failed").
//	F1 — a finally step's outputs were never promoted to job outputs: the
//	     promotion loop scanned c.Stages only.
//	F5 — `retry:` on a finally step degraded to a single attempt on a cancelled
//	     run, and the "failed to execute" diagnostic was suppressed, leaving a
//	     genuinely broken cleanup step with an empty log.
//
// Every test here drives the REAL shared orchestration loop (RunClaim) against
// a fake controller and a scripted ExecBackend — no shell, no container
// runtime — so the assertions are about orchestration semantics only and are
// identical on every platform. Both production agents call this same RunClaim,
// so a fix proven here is a fix on both backends.

// ---------------------------------------------------------------------------
// Backend
// ---------------------------------------------------------------------------

// finallySemBackend is a scripted ExecBackend. It embeds probeBackend
// (orchestrator_panic_test.go) for the inert defaults and overrides only the
// three seams these tests care about:
//
//   - RunDefault, so a test can script per-step behaviour (block, fail, exit
//     non-zero) and observe the exact context the orchestrator handed the step.
//   - StepLogWriters, so per-step log output is capturable (probeBackend
//     discards it, and F5 asserts on a log line).
//   - RunPostHook, so a test can observe the context the hook DRAIN runs on.
type finallySemBackend struct {
	probeBackend

	// runFn scripts each step. Required.
	runFn func(ctx context.Context, step api.ClaimStep, stdout, stderr io.Writer) (int, error)
	// postHookFn, when set, observes each drained post: hook's context.
	postHookFn func(ctx context.Context)

	mu   sync.Mutex
	logs map[int]*bytes.Buffer
}

func newFinallySemBackend(runFn func(ctx context.Context, step api.ClaimStep, stdout, stderr io.Writer) (int, error)) *finallySemBackend {
	return &finallySemBackend{runFn: runFn, logs: map[int]*bytes.Buffer{}}
}

func (b *finallySemBackend) RunDefault(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return b.runFn(ctx, step, stdout, stderr)
}

func (b *finallySemBackend) RunPostHook(ctx context.Context, scope ScopeHandle, container, script string, shell []string, env []string, stdout, stderr io.Writer) error {
	if b.postHookFn != nil {
		b.postHookFn(ctx)
	}
	return nil
}

func (b *finallySemBackend) StepLogWriters(ctx context.Context, stepIndex int) (io.Writer, io.Writer, func(ctx context.Context)) {
	b.mu.Lock()
	buf, ok := b.logs[stepIndex]
	if !ok {
		buf = &bytes.Buffer{}
		b.logs[stepIndex] = buf
	}
	b.mu.Unlock()
	w := &lockedWriter{mu: &b.mu, w: buf}
	return w, w, func(context.Context) {}
}

// stepLog returns everything written to stepIndex's writers so far.
func (b *finallySemBackend) stepLog(stepIndex int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if buf, ok := b.logs[stepIndex]; ok {
		return buf.String()
	}
	return ""
}

// lockedWriter serializes writes from steps that may run concurrently
// (RunPipeline's Concurrent mode) into one buffer.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// ---------------------------------------------------------------------------
// Fake controller
// ---------------------------------------------------------------------------

// finallySemHarness is a fake controller recording exactly what these four
// defects are observable through: each step's terminal status, the FinishRun
// status, and the SetRunOutputs bodies. Its GetRun handler serves a run status
// the test controls, so a test can drive the cancel poller (the only way
// cancelledByMaster is ever set) without any real cancellation plumbing.
type finallySemHarness struct {
	agentID string
	runID   string
	srv     *httptest.Server

	mu             sync.Mutex
	runStatus      api.RunStatus
	terminalStatus map[string]string
	runOutputs     []map[string]string
	finish         string

	finishCh chan string
}

func newFinallySemHarness(t *testing.T, agentID, runID string) *finallySemHarness {
	t.Helper()
	h := &finallySemHarness{
		agentID:        agentID,
		runID:          runID,
		runStatus:      api.RunRunning,
		terminalStatus: map[string]string{},
		finishCh:       make(chan string, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.StepName != "" && isTerminal(req.Status) {
			h.mu.Lock()
			h.terminalStatus[req.StepName] = req.Status
			h.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/outputs", func(w http.ResponseWriter, r *http.Request) {
		var req api.SetOutputsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.mu.Lock()
		h.runOutputs = append(h.runOutputs, req.Outputs)
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.finish = body.Status
		h.mu.Unlock()
		select {
		case h.finishCh <- body.Status:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/{runId}", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		st := h.runStatus
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Run{ID: r.PathValue("runId"), Status: st})
	})

	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

// cancelRun flips the status GetRun serves, which is what the cancel poller
// inside RunClaim observes to set cancelledByMaster.
func (h *finallySemHarness) cancelRun() {
	h.mu.Lock()
	h.runStatus = api.RunCancelled
	h.mu.Unlock()
}

func (h *finallySemHarness) client() *Client { return NewClient(h.srv.URL, "tok") }

func (h *finallySemHarness) stepStatus(name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.terminalStatus[name]
}

func (h *finallySemHarness) finishStatus() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.finish
}

// promotedRunOutputs merges every SetRunOutputs body the controller received.
func (h *finallySemHarness) promotedRunOutputs() map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]string{}
	for _, body := range h.runOutputs {
		for k, v := range body {
			out[k] = v
		}
	}
	return out
}

// shortenPollIntervals makes the cancel poller tick fast enough that a test
// never waits through a real 5s interval. RunClaim snapshots CancelPollInterval
// on its own goroutine, so this must be set before RunClaim is called.
func shortenPollIntervals(t *testing.T) {
	t.Helper()
	prev := CancelPollInterval
	CancelPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { CancelPollInterval = prev })
}

// shrinkFinallyBudget sets the per-phase cleanup ceiling for one test, so a
// budget assertion never has to wait out the real ten-minute default. Modeled
// on how internal/k8sagent's tests shrink stderrAutoFlushInterval.
func shrinkFinallyBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := FinallyBudget
	FinallyBudget = d
	t.Cleanup(func() { FinallyBudget = prev })
}

// runClaimWithDeadline drives RunClaim and fails the test if it does not
// return. The bound is a TEST watchdog, not an assertion about the fix: no
// test in this file asserts on elapsed wall-clock time. It exists so an
// unbounded phase surfaces as a readable failure rather than as a hung
// package-level `go test` timeout.
func runClaimWithDeadline(t *testing.T, h *finallySemHarness, c api.ClaimResponse, b ExecBackend) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunClaim(context.Background(), h.client(), h.agentID, c, b)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("RunClaim did not return: the finally phase is unbounded")
	}
}

// ---------------------------------------------------------------------------
// F4 — the finally phase and the hook drains must be bounded
// ---------------------------------------------------------------------------

// TestRunClaim_FinallyPhase_CarriesADeadline is the primary F4 regression
// test, and it is deliberately written so that it COMPILES AND RUNS against
// the pre-fix orchestrator: it names no new symbol, shrinks no budget, and
// measures no elapsed time. It asserts a single structural property of the
// contexts RunClaim hands to the cleanup phases — that they carry a deadline
// at all.
//
// That property is the whole defect. context.WithoutCancel(ctx) returns a
// context with no deadline AND a nil Done() channel, so every `select` on
// ctx.Done() inside a finally step (ExecuteCallStep's poll loop, a backend's
// exec wait) blocks forever. Before the fix, both assertions below report
// "no deadline" and the test fails.
func TestRunClaim_FinallyPhase_CarriesADeadline(t *testing.T) {
	h := newFinallySemHarness(t, "finally-deadline-agent", "run-finally-deadline")

	var mainHadDeadline, finallyHadDeadline atomic.Bool
	var hookHadDeadline atomic.Bool

	b := newFinallySemBackend(func(ctx context.Context, step api.ClaimStep, stdout, stderr io.Writer) (int, error) {
		_, ok := ctx.Deadline()
		switch step.Name {
		case "main":
			mainHadDeadline.Store(ok)
		case "cleanup":
			finallyHadDeadline.Store(ok)
		}
		return 0, nil
	})
	b.postHookFn = func(ctx context.Context) {
		_, ok := ctx.Deadline()
		hookHadDeadline.Store(ok)
	}

	claim := api.ClaimResponse{
		RunID:   h.runID,
		JobName: "finally-deadline",
		Native:  true,
		// No spec.timeoutMinutes: this is the common case, and it is exactly
		// the case that used to be unbounded. (With timeoutMinutes set the
		// MAIN DAG is bounded, but WithoutCancel drops that deadline too, so
		// finally: was unbounded either way.)
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "main", Run: "x"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 1, StageIndex: 0, Name: "cleanup", Run: "y",
				Post: &api.PostStep{Run: "z"},
			}},
		},
	}

	runClaimWithDeadline(t, h, claim, b)

	require.Equal(t, "Succeeded", h.finishStatus())
	assert.False(t, mainHadDeadline.Load(),
		"a main-DAG step in a job with no spec.timeoutMinutes must NOT gain a deadline: the budget bounds cleanup, not the job")
	assert.True(t, finallyHadDeadline.Load(),
		"a finally: step's context must carry the phase budget as a deadline; without one its Done() is nil and any wait inside it (e.g. ExecuteCallStep's child-run poll) blocks forever")
	assert.True(t, hookHadDeadline.Load(),
		"the post:/cache: hook drain runs on context.WithoutCancel for the same reason as finally: and must carry the same budget; RunPostHook has no timeout of its own")
}

// TestRunClaim_FinallyBudget_EndsAHangingFinallyStep is the behavioural half
// of F4: a finally step that never returns on its own must still let the run
// reach a terminal status. It asserts on OUTCOMES (the step's context was
// released by its deadline, RunClaim returned, FinishRun was sent) and never
// on elapsed wall-clock time — the budget is shrunk instead, the way
// internal/k8sagent's tests shrink stderrAutoFlushInterval.
func TestRunClaim_FinallyBudget_EndsAHangingFinallyStep(t *testing.T) {
	shrinkFinallyBudget(t, 150*time.Millisecond)

	h := newFinallySemHarness(t, "finally-budget-agent", "run-finally-budget")

	// releasedByDeadline records WHY the hanging step returned. The long
	// fallback timer is a safety valve so a regression fails with a readable
	// assertion instead of hanging; the assertion is on which arm fired, not
	// on how long it took.
	var releasedByDeadline atomic.Bool

	b := newFinallySemBackend(func(ctx context.Context, step api.ClaimStep, stdout, stderr io.Writer) (int, error) {
		if step.Name != "hang" {
			return 0, nil
		}
		select {
		case <-ctx.Done():
			releasedByDeadline.Store(true)
			return -1, ctx.Err()
		case <-time.After(30 * time.Second):
			return -1, fmt.Errorf("step was never released by its context")
		}
	})

	claim := api.ClaimResponse{
		RunID:   h.runID,
		JobName: "finally-budget",
		Native:  true,
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "main", Run: "x"}},
		},
		Finally: []api.ClaimStage{
			// No timeoutMinutes: per-step timeouts already worked inside
			// finally: — what was missing was the ceiling for authors who set
			// none.
			{Step: &api.ClaimStep{Index: 1, StageIndex: 0, Name: "hang", Run: "sleep-forever"}},
		},
	}

	runClaimWithDeadline(t, h, claim, b)

	assert.True(t, releasedByDeadline.Load(),
		"a finally: step that never returns must be released by the phase budget's deadline")
	assert.Equal(t, "Failed", h.finishStatus(),
		"a finally step killed by the phase budget is a finally failure, so the run must finish Failed, not hang or silently succeed")
}

// ---------------------------------------------------------------------------
// F3 — a failing finally step fails the run even when the run was cancelled
// ---------------------------------------------------------------------------

// TestRunClaim_FinallyFailure_OnCancelledRun_FailsRun reproduces F3: a user
// cancels a run, and a `finally:` step then exits non-zero because cleanup
// genuinely broke. Before the fix the step's "Failed" was rewritten to
// "Cancelled" whenever cancelledByMaster was set — regardless of phase — and
// because the failure is only recorded under `status == "Failed"`,
// recordFailure never fired and the entire suppressOnCancel=false mechanism
// was dead code for `run:` finally steps. The operator saw a clean
// cancellation over broken teardown.
//
// Both halves are asserted: the step's own terminal status, and the run's.
func TestRunClaim_FinallyFailure_OnCancelledRun_FailsRun(t *testing.T) {
	shortenPollIntervals(t)

	h := newFinallySemHarness(t, "finally-cancel-fail-agent", "run-finally-cancel-fail")

	b := newFinallySemBackend(func(ctx context.Context, step api.ClaimStep, stdout, stderr io.Writer) (int, error) {
		switch step.Name {
		case "main":
			// Cancel the run from the controller side, then wait to be
			// interrupted — the ordinary shape of a user-cancelled run.
			h.cancelRun()
			<-ctx.Done()
			return -1, ctx.Err()
		case "cleanup":
			// Genuine cleanup failure: a non-zero exit, no exec error. This
			// step's own context was never cancelled (finally: runs on a
			// non-cancelling context), so nothing about it is "cancelled".
			return 1, nil
		}
		return 0, nil
	})

	claim := api.ClaimResponse{
		RunID:   h.runID,
		JobName: "finally-cancel-fail",
		Native:  true,
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "main", Run: "x"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 1, StageIndex: 0, Name: "cleanup", Run: "y"}},
		},
	}

	runClaimWithDeadline(t, h, claim, b)

	assert.Equal(t, "Failed", h.stepStatus("cleanup"),
		"a finally step that exited non-zero must be reported Failed, not masked as Cancelled: its own context was never cancelled")
	assert.Equal(t, string(api.RunFailed), h.finishStatus(),
		"dsl.Spec.Finally documents that a finally step which fails marks the run Failed; that must hold on a cancelled run too, which is the whole point of makeStepRunner's suppressOnCancel=false")
}

// TestRunClaim_FinallySuccess_OnCancelledRun_StaysCancelled is the guard on
// the other side of F3: making the rewrite phase-aware must not turn every
// cancelled run into a failed one. A finally block that cleans up
// SUCCESSFULLY on a cancelled run still finishes Cancelled, and the main
// step interrupted by the cancellation is still reported Cancelled, not
// Failed.
func TestRunClaim_FinallySuccess_OnCancelledRun_StaysCancelled(t *testing.T) {
	shortenPollIntervals(t)

	h := newFinallySemHarness(t, "finally-cancel-ok-agent", "run-finally-cancel-ok")

	b := newFinallySemBackend(func(ctx context.Context, step api.ClaimStep, stdout, stderr io.Writer) (int, error) {
		if step.Name == "main" {
			h.cancelRun()
			<-ctx.Done()
			return -1, ctx.Err()
		}
		return 0, nil
	})

	claim := api.ClaimResponse{
		RunID:   h.runID,
		JobName: "finally-cancel-ok",
		Native:  true,
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "main", Run: "x"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 1, StageIndex: 0, Name: "cleanup", Run: "y"}},
		},
	}

	runClaimWithDeadline(t, h, claim, b)

	assert.Equal(t, "Cancelled", h.stepStatus("main"),
		"the main-DAG step interrupted by the cancellation must still be reported Cancelled, not Failed")
	assert.Equal(t, "Succeeded", h.stepStatus("cleanup"))
	assert.Equal(t, string(api.RunCancelled), h.finishStatus(),
		"a cancelled run whose cleanup succeeded stays Cancelled")
}

// ---------------------------------------------------------------------------
// F1 — a finally step's outputs are promoted to job outputs
// ---------------------------------------------------------------------------

// TestRunClaim_FinallyStepOutputs_PromotedToRunOutputs reproduces F1: a job
// declares spec.params.outputs and a `finally:` step sets one. SetStepOutputs
// lands, the run succeeds — and before the fix SetRunOutputs never carried the
// value, because the promotion loop iterated c.Stages only. A parent `call:`
// step reading that output got nothing.
func TestRunClaim_FinallyStepOutputs_PromotedToRunOutputs(t *testing.T) {
	h := newFinallySemHarness(t, "finally-outputs-agent", "run-finally-outputs")

	b := newFinallySemBackend(func(ctx context.Context, step api.ClaimStep, stdout, stderr io.Writer) (int, error) {
		return 0, nil
	})

	claim := api.ClaimResponse{
		RunID:      h.runID,
		JobName:    "finally-outputs",
		Native:     true,
		JobOutputs: []string{"report_url"},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "main", Run: "x"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 1, StageIndex: 0, Name: "publish", Run: "y",
				Outputs: map[string]string{"report_url": "https://reports.example/r/1"},
			}},
		},
	}

	runClaimWithDeadline(t, h, claim, b)

	require.Equal(t, "Succeeded", h.finishStatus())
	got := h.promotedRunOutputs()
	assert.Equal(t, "https://reports.example/r/1", got["report_url"],
		"an output declared by the job and set by a finally: step must be promoted to the run's outputs; the promotion loop must scan c.Finally as well as c.Stages")
}

// TestRunClaim_JobOutputs_FinallyWinsNameCollision pins the collision policy
// chosen for F1: when a main-DAG step and a finally step both declare the same
// output name, the FINALLY value wins.
//
// This is not a special case invented for finally: the promotion loop already
// resolved collisions between two main-DAG steps as "last writer wins" (a
// later stage overwrites an earlier one), and finally runs last. Keeping one
// rule — "the value promoted is the one set by the step that ran last" —
// avoids two rules that disagree at the phase boundary, and matches the useful
// direction: a teardown step recording what was actually left live overrides a
// provisional value from the main DAG, not the other way round.
func TestRunClaim_JobOutputs_FinallyWinsNameCollision(t *testing.T) {
	h := newFinallySemHarness(t, "finally-outputs-collision-agent", "run-finally-outputs-collision")

	b := newFinallySemBackend(func(ctx context.Context, step api.ClaimStep, stdout, stderr io.Writer) (int, error) {
		return 0, nil
	})

	claim := api.ClaimResponse{
		RunID:      h.runID,
		JobName:    "finally-outputs-collision",
		Native:     true,
		JobOutputs: []string{"report_url"},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, StageIndex: 0, Name: "main", Run: "x",
				Outputs: map[string]string{"report_url": "https://reports.example/provisional"},
			}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 1, StageIndex: 0, Name: "publish", Run: "y",
				Outputs: map[string]string{"report_url": "https://reports.example/final"},
			}},
		},
	}

	runClaimWithDeadline(t, h, claim, b)

	require.Equal(t, "Succeeded", h.finishStatus())
	got := h.promotedRunOutputs()
	assert.Equal(t, "https://reports.example/final", got["report_url"],
		"on a name collision the finally: step's value wins (last writer, matching how two main-DAG steps already resolve)")
}

// ---------------------------------------------------------------------------
// F5 — retry: on a finally step keeps its full attempt budget on a cancelled run
// ---------------------------------------------------------------------------

// TestRunClaim_FinallyRetry_OnCancelledRun_UsesEveryAttempt reproduces F5: the
// attempt loop's `if cancelledByMaster.Load() { break }` was consulted
// globally, so a `retry:` on a finally step silently degraded to one attempt
// exactly when cleanup mattered most — even though the finally pipeline runs
// on a non-cancelling context and its failures are genuine.
//
// It also covers the related diagnostic suppression: the "step failed to
// execute: ..." line is skipped on a cancelled run, which for a finally step
// meant a real exec error produced a completely empty log.
func TestRunClaim_FinallyRetry_OnCancelledRun_UsesEveryAttempt(t *testing.T) {
	shortenPollIntervals(t)

	h := newFinallySemHarness(t, "finally-retry-agent", "run-finally-retry")

	var cleanupAttempts atomic.Int32

	b := newFinallySemBackend(func(ctx context.Context, step api.ClaimStep, stdout, stderr io.Writer) (int, error) {
		switch step.Name {
		case "main":
			h.cancelRun()
			<-ctx.Done()
			return -1, ctx.Err()
		case "cleanup":
			cleanupAttempts.Add(1)
			// A non-nil error is an EXEC failure (the process never ran to
			// completion), which is the case whose diagnostic was suppressed.
			return -1, fmt.Errorf("teardown target unreachable")
		}
		return 0, nil
	})

	claim := api.ClaimResponse{
		RunID:   h.runID,
		JobName: "finally-retry",
		Native:  true,
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "main", Run: "x"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 1, StageIndex: 0, Name: "cleanup", Run: "y",
				Retry: &dsl.RetrySpec{Attempts: 3, Backoff: "1ms"},
			}},
		},
	}

	runClaimWithDeadline(t, h, claim, b)

	assert.Equal(t, int32(3), cleanupAttempts.Load(),
		"a finally: step's retry: must use its full attempt budget on a cancelled run; the cancellation ends the MAIN DAG, not the cleanup phase")
	assert.Contains(t, b.stepLog(1), "failed to execute",
		"a finally: step's genuine exec error must still be surfaced on its own stderr; suppressing it on a cancelled run leaves the step with an empty log and nothing to debug")
	assert.Equal(t, string(api.RunFailed), h.finishStatus(),
		"the finally step exhausted its retries and failed, so the run is Failed")
}

// TestRunClaim_MainRetry_OnCancelledRun_StopsImmediately is the guard on the
// other side of F5: making the break phase-aware must not make a CANCELLED
// main-DAG step keep retrying. A user who cancels a run expects it to stop,
// not to grind through the remaining attempts.
func TestRunClaim_MainRetry_OnCancelledRun_StopsImmediately(t *testing.T) {
	shortenPollIntervals(t)

	h := newFinallySemHarness(t, "main-retry-cancel-agent", "run-main-retry-cancel")

	var attempts atomic.Int32

	b := newFinallySemBackend(func(ctx context.Context, step api.ClaimStep, stdout, stderr io.Writer) (int, error) {
		attempts.Add(1)
		h.cancelRun()
		<-ctx.Done()
		return -1, ctx.Err()
	})

	claim := api.ClaimResponse{
		RunID:   h.runID,
		JobName: "main-retry-cancel",
		Native:  true,
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, StageIndex: 0, Name: "flaky", Run: "x",
				Retry: &dsl.RetrySpec{Attempts: 5, Backoff: "1ms"},
			}},
		},
	}

	runClaimWithDeadline(t, h, claim, b)

	assert.Equal(t, int32(1), attempts.Load(),
		"a master/user cancellation must still stop a MAIN-DAG step's retry loop at the current attempt")
	assert.Equal(t, string(api.RunCancelled), h.finishStatus())
}
