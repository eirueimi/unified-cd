package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerHandler returns 204 for /api/v1/agents/{id}/register.
func registerHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// stepHandler returns 204 for /api/v1/agents/{id}/steps.
func stepHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// finishHandler returns 204 for /api/v1/agents/{id}/runs/{runID}/finish.
func finishHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func claimResp(runID, script string) api.ClaimResponse {
	return api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test",
		Stages:  []api.ClaimStage{{Step: &api.ClaimStep{Name: "s1", Index: 0, Run: script}}},
	}
}

func TestAgent_GracefulDrain(t *testing.T) {
	// run-1: step sleeps for 300ms. Cancel claimCtx after 100ms and verify
	// that drain waits for the run to complete.
	finishCalled := make(chan struct{}, 1)
	stepStarted := make(chan struct{}, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", registerHandler)
	claimCount := 0
	var mu sync.Mutex
	mux.HandleFunc("POST /api/v1/agents/a1/claim", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := claimCount
		claimCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 0 {
			json.NewEncoder(w).Encode(claimResp("run-1", "sleep 0.3")) //nolint:errcheck
		} else {
			<-r.Context().Done()
			json.NewEncoder(w).Encode(api.ClaimResponse{}) //nolint:errcheck
		}
	})
	mux.HandleFunc("POST /api/v1/agents/a1/steps", func(w http.ResponseWriter, r *http.Request) {
		select {
		case stepStarted <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-1/finish", func(w http.ResponseWriter, r *http.Request) {
		select {
		case finishCalled <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	claimCtx, cancelClaim := context.WithCancel(context.Background())
	defer cancelClaim()

	a := &Agent{ID: "a1", Client: NewClient(srv.URL, "tok"), MaxConcurrent: 1}

	done := make(chan error, 1)
	go func() { done <- a.Run(claimCtx) }()

	// Wait for the step to start, then simulate SIGTERM
	select {
	case <-stepStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("step did not start")
	}
	cancelClaim()

	// Verify that Run() does not return immediately (drain in progress)
	select {
	case <-done:
		t.Fatal("Run() returned before drain completed")
	case <-time.After(50 * time.Millisecond):
	}

	// Verify that FinishRun is called and Run() returns
	select {
	case <-finishCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun was not called")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after drain")
	}
}

func TestAgent_DrainTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TODO: retryUntilSuccess with context.WithoutCancel keeps retrying after test server closes; Windows socket cleanup slower than Linux")
	}
	// DrainTimeout=200ms. The step sleeps for 60s (long).
	// Cancelling claimCtx after 50ms should cause Run() to return after 200ms.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", registerHandler)
	claimCount := 0
	var mu sync.Mutex
	mux.HandleFunc("POST /api/v1/agents/a2/claim", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := claimCount
		claimCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 0 {
			json.NewEncoder(w).Encode(claimResp("run-2", "sleep 60")) //nolint:errcheck
		} else {
			<-r.Context().Done()
			json.NewEncoder(w).Encode(api.ClaimResponse{}) //nolint:errcheck
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	claimCtx, cancelClaim := context.WithCancel(context.Background())

	a := &Agent{
		ID:            "a2",
		Client:        NewClient(srv.URL, "tok"),
		MaxConcurrent: 1,
		DrainTimeout:  200 * time.Millisecond,
	}

	done := make(chan error, 1)
	go func() { done <- a.Run(claimCtx) }()

	time.Sleep(100 * time.Millisecond) // wait for the run to start
	start := time.Now()
	cancelClaim()

	select {
	case <-done:
		elapsed := time.Since(start)
		// Run() should return shortly after DrainTimeout (200ms), not wait out
		// the 60s step sleep. The margin is generous (3s) because shared CI
		// runners schedule the drain goroutine with enough latency that a
		// tighter bound flakes — 1.2s was observed on ubuntu/macos — while 3s
		// still cleanly distinguishes "drained" from "waited for the 60s sleep".
		assert.Less(t, elapsed, 3*time.Second, "Run() should return within DrainTimeout, not wait out the step")
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not return after DrainTimeout")
	}
}

func TestAgent_MaxConcurrent(t *testing.T) {
	// Verify that N=2 allows 2 runs to execute concurrently.
	// Use a barrier file to prevent completion until both runs have started.
	wsDir := t.TempDir()
	barrier := filepath.Join(t.TempDir(), "barrier")
	require.NoError(t, os.WriteFile(barrier, []byte("wait"), 0o644))

	var inFlight atomic.Int32
	var maxInFlight atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", registerHandler)
	claimCount := 0
	var mu sync.Mutex
	mux.HandleFunc("POST /api/v1/agents/a3/claim", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := claimCount
		claimCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 0:
			json.NewEncoder(w).Encode(claimResp("run-1", //nolint:errcheck
				"while [ -f "+barrier+" ]; do sleep 0.01; done"))
		case 1:
			json.NewEncoder(w).Encode(claimResp("run-2", //nolint:errcheck
				"while [ -f "+barrier+" ]; do sleep 0.01; done"))
		default:
			<-r.Context().Done()
			json.NewEncoder(w).Encode(api.ClaimResponse{}) //nolint:errcheck
		}
	})
	mux.HandleFunc("POST /api/v1/agents/a3/steps", func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a3/runs/run-1/finish", func(w http.ResponseWriter, r *http.Request) {
		inFlight.Add(-1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a3/runs/run-2/finish", func(w http.ResponseWriter, r *http.Request) {
		inFlight.Add(-1)
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	claimCtx, cancelClaim := context.WithCancel(context.Background())
	defer cancelClaim()

	a := &Agent{
		ID:            "a3",
		Client:        NewClient(srv.URL, "tok"),
		MaxConcurrent: 2,
		WorkspaceDir:  wsDir,
	}

	go a.Run(claimCtx) //nolint:errcheck

	// Wait until both runs are executing concurrently (up to 5s)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if maxInFlight.Load() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.GreaterOrEqual(t, maxInFlight.Load(), int32(2), "2 runs should execute concurrently")

	// Release the barrier to let the runs complete
	os.Remove(barrier) //nolint:errcheck
	cancelClaim()
}

func TestAgent_CleanWorkspace(t *testing.T) {
	// Verify that when CleanWorkspace=true, the workspace is recreated before each run.
	// Place a sentinel file and have the run's step confirm its absence.
	wsDir := t.TempDir()
	// claimResp sets JobName "test", so the per-job workDir is working0/test.
	jobDir := filepath.Join(wsDir, "working0", "test")
	require.NoError(t, os.MkdirAll(jobDir, 0o755))
	// sentinel: a file that should be deleted by CleanWorkspace
	require.NoError(t, os.WriteFile(filepath.Join(jobDir, "sentinel.txt"), []byte("dirty"), 0o644))

	// step: exit 1 if sentinel exists (test failure), exit 0 otherwise
	stepScript := "test ! -f sentinel.txt"

	runDone := make(chan bool, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", registerHandler)
	claimCount := 0
	var mu sync.Mutex
	mux.HandleFunc("POST /api/v1/agents/a4/claim", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := claimCount
		claimCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 0 {
			json.NewEncoder(w).Encode(claimResp("run-4", stepScript)) //nolint:errcheck
		} else {
			<-r.Context().Done()
			json.NewEncoder(w).Encode(api.ClaimResponse{}) //nolint:errcheck
		}
	})
	mux.HandleFunc("POST /api/v1/agents/a4/steps", stepHandler)
	mux.HandleFunc("POST /api/v1/agents/a4/runs/run-4/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Status string }
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		runDone <- body.Status == "Succeeded"
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	claimCtx, cancelClaim := context.WithCancel(context.Background())
	defer cancelClaim()

	a := &Agent{
		ID:             "a4",
		Client:         NewClient(srv.URL, "tok"),
		MaxConcurrent:  1,
		WorkspaceDir:   wsDir,
		CleanWorkspace: true,
	}

	go a.Run(claimCtx) //nolint:errcheck

	select {
	case succeeded := <-runDone:
		assert.True(t, succeeded, "step should succeed: sentinel was cleaned by CleanWorkspace")
	case <-time.After(10 * time.Second):
		t.Fatal("run did not complete")
	}
	cancelClaim()
}

// newTimeoutTestMux returns a minimal HTTP multiplexer for timeout tests.
// The status reported by FinishRun is sent to finishCh.
func newTimeoutTestMux(t *testing.T, agentID, runID string, finishCh chan<- string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/"+runID, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Run{ID: runID, Status: api.RunRunning}) //nolint:errcheck
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
	return mux
}

// TestAgent_StepTimeout verifies that step-level timeout works correctly.
func TestAgent_StepTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TODO: retryUntilSuccess with context.WithoutCancel keeps retrying after test server closes; Windows socket cleanup slower than Linux")
	}
	const agentID = "timeout-step-agent"
	const runID = "run-step-timeout"

	finishCh := make(chan string, 1)
	mux := newTimeoutTestMux(t, agentID, runID, finishCh)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := &Agent{
		ID:     agentID,
		Client: NewClient(srv.URL, "tok"),
	}

	resp := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-step-timeout",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:          0,
				Name:           "slow-step",
				Run:            "sleep 10",
				TimeoutMinutes: 0.001, // ~60ms
			}},
		},
	}

	start := time.Now()
	a.executeRun(context.Background(), resp, "")
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 2*time.Second, "step timeout should cause early termination")

	select {
	case status := <-finishCh:
		assert.Equal(t, "Failed", status, "a timed-out step should result in Failed")
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun was not called")
	}
}

// TestAgent_JobTimeout verifies that job-level timeout works correctly.
func TestAgent_JobTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TODO: retryUntilSuccess with context.WithoutCancel keeps retrying after test server closes; Windows socket cleanup slower than Linux")
	}
	const agentID = "timeout-job-agent"
	const runID = "run-job-timeout"

	finishCh := make(chan string, 1)
	mux := newTimeoutTestMux(t, agentID, runID, finishCh)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := &Agent{
		ID:     agentID,
		Client: NewClient(srv.URL, "tok"),
	}

	resp := api.ClaimResponse{
		Native:         true,
		RunID:          runID,
		JobName:        "test-job-timeout",
		TimeoutMinutes: 0.001, // ~60ms (job level)
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0,
				Name:  "slow-step",
				Run:   "sleep 10",
			}},
		},
	}

	start := time.Now()
	a.executeRun(context.Background(), resp, "")
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 2*time.Second, "job timeout should cause early termination")

	select {
	case status := <-finishCh:
		assert.Equal(t, "Failed", status, "a timed-out job should result in Failed")
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun was not called")
	}
}

// TestAgent_ExposesAgentOSEnvVar verifies that the UNIFIED_AGENT_OS environment variable
// (the value of runtime.GOOS) is automatically passed to step.run so job authors can detect
// the current OS, and that UNIFIED_WORKSPACE names the step's own cwd (native: workDir).
//
// The UNIFIED_WORKSPACE check uses a marker file placed in workDir rather than a literal
// `test "$UNIFIED_WORKSPACE" = "$PWD"` comparison: on Windows, RunStep's cmd.Dir is set to
// workDir as a native (backslash) path, but the Git Bash/MSYS shell that runs step.Run
// reports $PWD in its own POSIX-translated form (e.g. "/c/Users/..." or an MSYS mount-point
// alias) — the same directory, a different string. Resolving `$UNIFIED_WORKSPACE/marker`
// proves UNIFIED_WORKSPACE names the correct directory regardless of that platform-specific
// path-string encoding.
func TestAgent_ExposesAgentOSEnvVar(t *testing.T) {
	const agentID = "os-env-agent"
	const runID = "run-os-env"

	finishCh := make(chan string, 1)
	mux := newTimeoutTestMux(t, agentID, runID, finishCh)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := &Agent{
		ID:     agentID,
		Client: NewClient(srv.URL, "tok"),
	}

	workDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "marker"), []byte("x"), 0o644))

	resp := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-os-env",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, Name: "check-os", Run: `test "$UNIFIED_AGENT_OS" = "` + runtime.GOOS + `" && test -f "$UNIFIED_WORKSPACE/marker"`}},
		},
	}

	// A real workDir (not "") so the step's actual cwd is deterministic and
	// matches what the backend reports as UNIFIED_WORKSPACE (see
	// hostBackend.WorkspacePath: native mode reports b.workDir, which RunStep
	// also uses as cmd.Dir).
	a.executeRun(context.Background(), resp, workDir)

	select {
	case status := <-finishCh:
		assert.Equal(t, "Succeeded", status, "UNIFIED_AGENT_OS should match runtime.GOOS and UNIFIED_WORKSPACE should match $PWD")
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun was not called")
	}
}

// TestAgent_CallStep_NonexistentJob_FailsRun verifies that when call.job names a non-existent job,
// child Run creation fails with 404 and both the step and the Run are marked as Failed.
func TestAgent_CallStep_NonexistentJob_FailsRun(t *testing.T) {
	const agentID = "call-missing-job-agent"
	const runID = "run-call-missing-job"

	finishCh := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/runs", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "job not found: missing-job", http.StatusNotFound)
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

	a := &Agent{
		ID:     agentID,
		Client: NewClient(srv.URL, "tok"),
	}

	resp := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-call-missing-job",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0,
				Name:  "callMissing",
				Call:  &api.ClaimCallStep{Job: "missing-job"},
			}},
		},
	}

	a.executeRun(context.Background(), resp, "")

	select {
	case status := <-finishCh:
		assert.Equal(t, "Failed", status, "a call to a non-existent job should fail the Run")
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun was not called")
	}
}

// TestAgent_UploadArtifact_RelativePath verifies that a relative uploadArtifact path
// resolves against the run's workspace working directory (the same directory shell
// steps run in), not the agent process's cwd. It also verifies the artifact step's
// ReportStep calls carry the step name (and stage index), not a blank/zero value.
func TestAgent_UploadArtifact_RelativePath(t *testing.T) {
	const agentID = "upload-artifact-agent"
	const runID = "run-upload-artifact"

	// workDir simulates "<workspaceDir>/workingN" -- the per-run working directory
	// ExecStep/shell steps use as their cwd. Deliberately distinct from the process cwd.
	workDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "out.txt"), []byte("artifact contents"), 0o644))

	var uploadedBody []byte
	var uploadReceived bool
	var reportedStepNames []string
	var reportedStageIndexes []int

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var body api.StepReportRequest
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		reportedStepNames = append(reportedStepNames, body.StepName)
		reportedStageIndexes = append(reportedStageIndexes, body.StageIndex)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /api/v1/runs/"+runID+"/artifacts/out", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		uploadedBody = b
		uploadReceived = true
		w.WriteHeader(http.StatusNoContent)
	})
	finishCh := make(chan string, 1)
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

	a := &Agent{
		ID:     agentID,
		Client: NewClient(srv.URL, "tok"),
	}

	resp := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-upload-artifact",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:      0,
				StageIndex: 0,
				Name:       "uploadOut",
				UploadArtifact: &api.UploadArtifactStep{
					Name: "out",
					Path: "out.txt", // relative path, as job docs show
				},
			}},
		},
	}

	// Process cwd must NOT contain out.txt -- otherwise the (buggy) old behavior
	// of resolving against os.Getwd() would also succeed and the test wouldn't
	// catch the regression.
	cwd, err := os.Getwd()
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(cwd, "out.txt"))
	require.True(t, os.IsNotExist(statErr), "precondition: out.txt must not exist in process cwd")

	a.executeRun(context.Background(), resp, workDir)

	select {
	case status := <-finishCh:
		assert.Equal(t, "Succeeded", status, "upload-artifact of a relative path under the workspace dir should succeed")
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun was not called")
	}

	assert.True(t, uploadReceived, "artifact upload endpoint should have been hit")
	assert.NotEmpty(t, uploadedBody, "uploaded artifact body should not be empty")

	// The ReportStep calls for the artifact step should carry the step's name and
	// stage index, so `run show` displays "uploadOut" rather than "step[0]".
	require.NotEmpty(t, reportedStepNames)
	for _, name := range reportedStepNames {
		assert.Equal(t, "uploadOut", name, "artifact ReportStep calls should carry the step name")
	}
	for _, idx := range reportedStageIndexes {
		assert.Equal(t, 0, idx, "artifact ReportStep calls should carry the stage index")
	}
}
