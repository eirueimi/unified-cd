package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// This file is the regression suite for "hooks registered by a `finally:`
// step are silently dropped".
//
// RunClaim (orchestrator.go) used to drain postHooks (cache saves) and
// hookStack (post: hooks) exactly once, between the main DAG and the
// `finally` pipeline — and never again. Any hook a `finally:` step registered
// therefore landed on a stack nobody ever popped: the post: script never ran,
// the cache: save never happened, and nothing anywhere reported the omission
// (not the step status, not the run status, not even an agent-side warning).
//
// Both production backends share this one orchestration loop (the k8s agent
// calls agentlib.RunClaim too — see internal/k8sagent/agent.go), so the bug
// and its fix are backend-independent; the parity suite gains
// `finally-post-hook-runs` to keep the two honest about it.

// scriptPath makes a host path safe to embed in a shell script: on Windows the
// script is interpreted by git-bash (findShell), which reads a raw backslash
// Windows path as escape sequences. Mirrors the helper inlined in
// TestExecuteRun_ParallelPostHooks_ConcurrentAppendIsSafe.
func scriptPath(p string) string { return strings.ReplaceAll(p, "\\", "/") }

// finallyHookHarness is a mock controller for the tests below: it accepts
// registration/step reports/log bulk for any step index, and captures the
// terminal FinishRun status.
type finallyHookHarness struct {
	agentID  string
	runID    string
	mux      *http.ServeMux
	srv      *httptest.Server
	finishCh chan string

	mu       sync.Mutex
	logLines map[int][]string
	statuses map[string]string
}

func newFinallyHookHarness(t *testing.T, agentID, runID string) *finallyHookHarness {
	t.Helper()
	h := &finallyHookHarness{
		agentID:  agentID,
		runID:    runID,
		finishCh: make(chan string, 1),
		logLines: map[int][]string{},
		statuses: map[string]string{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.StepName != "" {
			h.mu.Lock()
			h.statuses[req.StepName] = req.Status
			h.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// One wildcard handler for every step index, so a test never has to know
	// how many step log streams the claim will open.
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var idx int
		_, _ = fmt.Sscanf(r.PathValue("idx"), "%d", &idx)
		var reqs []api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&reqs)
		h.mu.Lock()
		for _, req := range reqs {
			if req.Line != "" {
				h.logLines[idx] = append(h.logLines[idx], req.Line)
			}
		}
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		select {
		case h.finishCh <- body.Status:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})

	h.mux = mux
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *finallyHookHarness) agent() *Agent {
	return &Agent{ID: h.agentID, Client: NewClient(h.srv.URL, "tok")}
}

func (h *finallyHookHarness) finishStatus(t *testing.T) string {
	t.Helper()
	select {
	case s := <-h.finishCh:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun was not called")
		return ""
	}
}

func (h *finallyHookHarness) stepLog(idx int) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.logLines[idx], "\n")
}

func (h *finallyHookHarness) stepStatus(name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.statuses[name]
}

// TestExecuteRun_FinallyStepPostHook_Runs is the core regression test: a
// `post:` hook on a step inside `finally:` must actually execute, and its
// output must be attributed to the owning finally step's log index exactly
// like a main-DAG step's post hook is.
func TestExecuteRun_FinallyStepPostHook_Runs(t *testing.T) {
	h := newFinallyHookHarness(t, "finally-post-agent", "run-finally-post")
	workDir := t.TempDir()
	marker := filepath.Join(workDir, "finally-post-marker.txt")

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   h.runID,
		JobName: "finally-post",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "main", Run: "echo main-ran"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 1, StageIndex: 0, Name: "cleanup", Run: "echo cleanup-ran",
				Post: &api.PostStep{Run: "echo FINALLY_POST_MARKER; echo posted > \"" + scriptPath(marker) + "\""},
			}},
		},
	}

	h.agent().executeRun(context.Background(), claim, workDir)

	assert.Equal(t, "Succeeded", h.finishStatus(t))
	assert.Equal(t, "Succeeded", h.stepStatus("cleanup"))

	_, err := os.Stat(marker)
	require.NoError(t, err, "a post: hook on a finally: step must run (its marker file must exist)")

	assert.Contains(t, h.stepLog(1), "FINALLY_POST_MARKER",
		"a finally step's post-hook output must reach the OWNING finally step's log")
}

// TestExecuteRun_FinallyStepPostHook_RunsWhenMainFailed pins the case
// `finally:` exists for: the main DAG already failed. The finally step still
// runs, so its post: hook must still run — the surrounding machinery winding
// down must not swallow it.
func TestExecuteRun_FinallyStepPostHook_RunsWhenMainFailed(t *testing.T) {
	h := newFinallyHookHarness(t, "finally-post-fail-agent", "run-finally-post-fail")
	workDir := t.TempDir()
	marker := filepath.Join(workDir, "finally-post-fail-marker.txt")

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   h.runID,
		JobName: "finally-post-fail",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "boom", Run: "exit 1"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 1, StageIndex: 0, Name: "cleanup", Run: "echo cleanup-ran",
				Post: &api.PostStep{Run: "echo posted > \"" + scriptPath(marker) + "\""},
			}},
		},
	}

	h.agent().executeRun(context.Background(), claim, workDir)

	assert.Equal(t, "Failed", h.finishStatus(t))
	assert.Equal(t, "Succeeded", h.stepStatus("cleanup"))

	_, err := os.Stat(marker)
	require.NoError(t, err, "a finally step's post: hook must run even when the main DAG failed")
}

// TestExecuteRun_FinallyStepCacheSave_Runs covers the OTHER hook slice: a
// `cache:` step registers its save into postHooks (executeCacheStep), not
// hookStack. A cache: step inside finally: must still get its deferred save.
func TestExecuteRun_FinallyStepCacheSave_Runs(t *testing.T) {
	h := newFinallyHookHarness(t, "finally-cache-agent", "run-finally-cache")
	workDir := t.TempDir()

	// Content the finally-block cache: step is expected to archive.
	built := filepath.Join(workDir, "built")
	require.NoError(t, os.MkdirAll(built, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(built, "built.txt"), []byte("artifact"), 0o644))

	a := h.agent()
	a.CacheStore = objectstore.NewLocalObjectStore(t.TempDir())

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   h.runID,
		JobName: "finally-cache",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "main", Run: "echo main-ran"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 1, StageIndex: 0, Name: "cache-it",
				Cache: &dsl.CacheStep{Path: "built", Key: "finally-cache-key"},
			}},
		},
	}

	a.executeRun(context.Background(), claim, workDir)

	assert.Equal(t, "Succeeded", h.finishStatus(t))

	restored := t.TempDir()
	hit, err := cache.Restore(context.Background(), a.CacheStore, "finally-cache", restored, "finally-cache-key", nil)
	require.NoError(t, err)
	require.True(t, hit, "a cache: step inside finally: must still get its deferred save drained")
	got, err := os.ReadFile(filepath.Join(restored, "built.txt"))
	require.NoError(t, err)
	assert.Equal(t, "artifact", string(got))
}

// TestExecuteRun_HookDrainOrder_MainBeforeFinally_LIFOWithinEach pins the
// ordering contract the fix must establish, in one observable sequence:
//
//   - a main-DAG step's post: hook drains BEFORE the finally pipeline starts
//     (a normal step's cleanup must not wait on finally:);
//   - a finally step's post: hook drains AFTER finally completes;
//   - within each drain batch the existing LIFO guarantee holds (the
//     last-registered hook of the batch fires first — see the
//     `post-hooks-lifo` parity case).
func TestExecuteRun_HookDrainOrder_MainBeforeFinally_LIFOWithinEach(t *testing.T) {
	h := newFinallyHookHarness(t, "finally-order-agent", "run-finally-order")
	workDir := t.TempDir()
	order := filepath.Join(workDir, "order.txt")
	appendMarker := func(tag string) string {
		return "echo " + tag + " >> \"" + scriptPath(order) + "\""
	}

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   h.runID,
		JobName: "finally-order",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, StageIndex: 0, Name: "main1", Run: "echo main1",
				Post: &api.PostStep{Run: appendMarker("main-post-1")},
			}},
			{Step: &api.ClaimStep{
				Index: 1, StageIndex: 1, Name: "main2", Run: "echo main2",
				Post: &api.PostStep{Run: appendMarker("main-post-2")},
			}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 2, StageIndex: 0, Name: "fin1", Run: appendMarker("finally-body-1"),
				Post: &api.PostStep{Run: appendMarker("finally-post-1")},
			}},
			{Step: &api.ClaimStep{
				Index: 3, StageIndex: 1, Name: "fin2", Run: appendMarker("finally-body-2"),
				Post: &api.PostStep{Run: appendMarker("finally-post-2")},
			}},
		},
	}

	h.agent().executeRun(context.Background(), claim, workDir)
	assert.Equal(t, "Succeeded", h.finishStatus(t))

	data, err := os.ReadFile(order)
	require.NoError(t, err, "order marker file should have been written")
	got := strings.Fields(strings.TrimSpace(string(data)))

	want := []string{
		// Main-DAG post hooks drain first, LIFO within the batch.
		"main-post-2", "main-post-1",
		// Then the finally pipeline body, in declaration order.
		"finally-body-1", "finally-body-2",
		// Then the finally batch's own post hooks, LIFO within the batch.
		"finally-post-2", "finally-post-1",
	}
	assert.Equal(t, want, got)
}

// TestExecuteRun_ParallelFinallyPostHooks_ConcurrentAppendIsSafe is the
// concurrency twin of TestExecuteRun_ParallelPostHooks_ConcurrentAppendIsSafe
// for the finally pipeline. finally: runs through the same RunPipeline with
// the same backend concurrency mode, so several finally: steps in a parallel:
// group register their hooks from concurrent goroutines. Under `-race` this
// proves the finally drain's take-ownership of the shared slices is properly
// serialized by postHooksMu; by checking every marker it also proves no entry
// was silently lost.
func TestExecuteRun_ParallelFinallyPostHooks_ConcurrentAppendIsSafe(t *testing.T) {
	h := newFinallyHookHarness(t, "finally-parallel-agent", "run-finally-parallel")
	workDir := t.TempDir()

	const n = 8
	members := make([]api.ClaimStep, n)
	markers := make([]string, n)
	for i := 0; i < n; i++ {
		markers[i] = filepath.Join(workDir, fmt.Sprintf("finally-post-%d.txt", i))
		members[i] = api.ClaimStep{
			Index:      i + 1,
			StageIndex: 0,
			Name:       fmt.Sprintf("fin-%d", i),
			Run:        "echo fin",
			Post:       &api.PostStep{Run: "echo posted > \"" + scriptPath(markers[i]) + "\""},
		}
	}

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   h.runID,
		JobName: "finally-parallel",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "main", Run: "echo main"}},
		},
		Finally: []api.ClaimStage{{Parallel: members}},
	}

	h.agent().executeRun(context.Background(), claim, workDir)
	assert.Equal(t, "Succeeded", h.finishStatus(t))

	for i, m := range markers {
		if _, err := os.Stat(m); err != nil {
			t.Errorf("finally parallel member %d: post hook did not run (marker %s missing): %v", i, m, err)
		}
	}
}

// TestExecuteRun_FinallyStepPostHook_RunsWhenCancelled is the hardest case:
// the run is cancelled at the controller while the main DAG is still going.
// finally: is defined to run anyway (RunClaim gives it a WithoutCancel
// context), so the hooks its steps register must drain on a non-cancelled
// context too — otherwise cleanup silently evaporates in exactly the
// situation it exists for.
func TestExecuteRun_FinallyStepPostHook_RunsWhenCancelled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TODO: retryUntilSuccess with context.WithoutCancel keeps retrying after the test server closes; Windows socket cleanup is slower than Linux (see TestExecuteRun_CancelPropagation)")
	}

	prevPoll := CancelPollInterval
	CancelPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { CancelPollInterval = prevPoll })

	h := newFinallyHookHarness(t, "finally-cancel-agent", "run-finally-cancel")
	workDir := t.TempDir()
	marker := filepath.Join(workDir, "finally-cancel-marker.txt")

	var getRunCalls atomic.Int32
	h.mux.HandleFunc("GET /api/v1/runs/"+h.runID, func(w http.ResponseWriter, r *http.Request) {
		status := api.RunRunning
		if getRunCalls.Add(1) >= 2 {
			status = api.RunCancelled
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Run{ID: h.runID, Status: status})
	})

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   h.runID,
		JobName: "finally-cancel",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "slow", Run: "sleep 30"}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 1, StageIndex: 0, Name: "cleanup", Run: "echo cleanup-ran",
				Post: &api.PostStep{Run: "echo posted > \"" + scriptPath(marker) + "\""},
			}},
		},
	}

	h.agent().executeRun(context.Background(), claim, workDir)

	assert.Equal(t, string(api.RunCancelled), h.finishStatus(t))
	_, err := os.Stat(marker)
	require.NoError(t, err, "a finally step's post: hook must run even when the run was cancelled")
}
