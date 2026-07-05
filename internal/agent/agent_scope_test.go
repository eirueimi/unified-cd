package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/klauspost/compress/zstd"
)

// makeAgentTestTarZstd builds a minimal tar+zstd stream, mirroring
// internal/artifact/targz_test.go's makeTarZstd helper, for tests that need
// Client.DownloadArtifact to succeed against a real artifact.ExtractTarZstd
// call rather than an empty/204 response body.
func makeAgentTestTarZstd(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestIsScopedStep(t *testing.T) {
	if !isScopedStep(api.ClaimStep{ScopeID: "scope:x"}) {
		t.Fatal("expected scoped")
	}
	if isScopedStep(api.ClaimStep{}) {
		t.Fatal("expected not scoped")
	}
}

// contentRT is a fakeRT variant whose CopyOut writes real, distinguishable
// file content to the host destination, so a test can prove an upload came
// from the scope container rather than from the host workspace.
type contentRT struct {
	fakeRT
	copyOutContent string
	copyOutCalls   int
	copyOutSrcPath string
}

func (c *contentRT) CopyOut(_ context.Context, _ crt.ContainerHandle, containerPath, hostPath string) error {
	c.copyOutCalls++
	c.copyOutSrcPath = containerPath
	return os.WriteFile(hostPath, []byte(c.copyOutContent), 0o644)
}

// TestExecuteRun_UploadArtifact_ScopedRoutesToScopeContainer verifies that when
// a step carries ScopeID, executeUploadArtifact takes the scope-container path
// (scopeManager.copyOutToTemp) instead of resolveWorkspacePath(workDir, ...).
// It asserts on the actual bytes reaching the upload endpoint: workDir holds a
// decoy file, and the fake runtime's CopyOut supplies distinct "scope content"
// bytes — only the scope path should ever reach the server.
func TestExecuteRun_UploadArtifact_ScopedRoutesToScopeContainer(t *testing.T) {
	const agentID = "scoped-upload-agent"
	const runID = "run-scoped-upload"

	workDir := t.TempDir()
	// Decoy: if routing regresses to the host workspace path, this is what
	// would be uploaded instead of the scope content.
	if err := os.WriteFile(filepath.Join(workDir, "out.txt"), []byte("host workspace decoy"), 0o644); err != nil {
		t.Fatal(err)
	}

	var uploadedBody []byte
	var uploadReceived bool

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /api/v1/runs/"+runID+"/artifacts/out", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
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

	rt := &contentRT{copyOutContent: "scope container content"}

	a := &Agent{
		ID:     agentID,
		Client: NewClient(srv.URL, "tok"),
		// Force containerRuntime() to resolve to our fake without touching
		// Detect()/RuntimePref: pre-seed the resolved runtime via runtimeOnce.
	}
	a.runtimeOnce.Do(func() {}) // mark as resolved
	a.resolvedRuntime = rt

	resp := api.ClaimResponse{
		RunID:   runID,
		JobName: "test-scoped-upload",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:      0,
				StageIndex: 0,
				Name:       "uploadOut",
				ScopeID:    "scope:build",
				ScopeImage: "golang:1.22",
				UploadArtifact: &api.UploadArtifactStep{
					Name: "out",
					Path: "out.txt",
				},
			}},
		},
	}

	a.executeRun(context.Background(), resp, workDir)

	select {
	case status := <-finishCh:
		if status != "Succeeded" {
			t.Fatalf("expected Succeeded, got %s", status)
		}
	default:
		t.Fatal("FinishRun was not called")
	}

	if !uploadReceived {
		t.Fatal("upload was never received")
	}
	if rt.copyOutCalls != 1 {
		t.Fatalf("expected exactly 1 CopyOut call, got %d", rt.copyOutCalls)
	}
	// Finding 1: a relative container-side artifact path must be resolved
	// against scopeWorkDir ("/workspace") before reaching CopyOut, mirroring
	// the k8s agent's path.Join(mountPath, ...). A raw relative path handed to
	// `docker cp` is rejected ("destination path must be absolute").
	if rt.copyOutSrcPath != "/workspace/out.txt" {
		t.Fatalf("expected CopyOut to be called with the absolute container path %q, got %q", "/workspace/out.txt", rt.copyOutSrcPath)
	}
	// The uploaded body is tar+zstd; just prove it's non-empty and did not
	// come from an untouched host workDir copy (which would fail to route
	// through copyOutToTemp at all, since contentRT.CopyOut is the only
	// source of "scope container content").
	if len(uploadedBody) == 0 {
		t.Fatal("expected non-empty uploaded artifact body")
	}
}

// copyInTrackingRT is a fakeRT variant that records the CONTAINER-side path
// passed to CopyIn, so a test can assert it was resolved to an absolute
// /workspace/... path (Finding 1) instead of being passed through relative
// (which docker/podman reject: "destination path must be absolute").
type copyInTrackingRT struct {
	fakeRT
	copyInCalls         int
	copyInContainerPath string
}

func (c *copyInTrackingRT) CopyIn(_ context.Context, _ crt.ContainerHandle, hostPath, containerPath string) error {
	c.copyInCalls++
	c.copyInContainerPath = containerPath
	return nil
}

// TestExecuteRun_DownloadArtifact_ScopedUsesAbsoluteContainerPath is the
// regression test for Finding 1 on the download-artifact path: a scoped
// download-artifact step with the default destDir (".") must resolve to an
// absolute /workspace path before scopeManager.copyIn, mirroring the k8s
// agent's path.Join(mountPath, dest). A relative "." handed straight to
// `docker cp` is rejected by docker/podman ("destination path must be
// absolute").
func TestExecuteRun_DownloadArtifact_ScopedUsesAbsoluteContainerPath(t *testing.T) {
	const agentID = "scoped-download-agent"
	const runID = "run-scoped-download"

	workDir := t.TempDir()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/"+runID+"/artifacts/in", func(w http.ResponseWriter, r *http.Request) {
		// Client-side DownloadArtifact extracts the response body as a real
		// tar+zstd stream (artifact.ExtractTarZstd), so serve a minimal valid
		// one rather than an empty/204 body.
		w.Write(makeAgentTestTarZstd(t, map[string]string{"in.txt": "content"})) //nolint:errcheck
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

	rt := &copyInTrackingRT{}
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = rt

	resp := api.ClaimResponse{
		RunID:   runID,
		JobName: "test-scoped-download",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:      0,
				StageIndex: 0,
				Name:       "downloadIn",
				ScopeID:    "scope:build",
				ScopeImage: "golang:1.22",
				DownloadArtifact: &api.DownloadArtifactStep{
					Name: "in",
					// DestDir intentionally empty: executeDownloadArtifact
					// defaults it to ".", the exact relative-path case Finding
					// 1 is about.
				},
			}},
		},
	}

	a.executeRun(context.Background(), resp, workDir)

	select {
	case status := <-finishCh:
		if status != "Succeeded" {
			t.Fatalf("expected Succeeded, got %s", status)
		}
	default:
		t.Fatal("FinishRun was not called")
	}

	if rt.copyInCalls != 1 {
		t.Fatalf("expected exactly 1 CopyIn call, got %d", rt.copyInCalls)
	}
	if rt.copyInContainerPath != "/workspace" {
		t.Fatalf("expected CopyIn to be called with the absolute container path %q (default destDir \".\" resolved against scopeWorkDir), got %q", "/workspace", rt.copyInContainerPath)
	}
}

// failingCreateRT is a fakeRT variant whose Create always errors, simulating
// a scope environment that can never be provisioned (e.g. the container
// runtime is unreachable, image pull fails, etc).
type failingCreateRT struct {
	fakeRT
}

func (f *failingCreateRT) Create(context.Context, crt.CreateSpec) (crt.ContainerHandle, error) {
	return crt.ContainerHandle{}, fmt.Errorf("simulated scope provisioning failure")
}

// TestExecuteRun_ScopedCache_ScopeUnavailable_WarnAndSkip is the regression
// test for Finding 2: on the host agent, a scoped cache step whose scope
// handle cannot be resolved must warn+skip (report Succeeded), matching both
// the host's existing lenient cache policy (a restore/save error never fails
// the step) and the k8s agent's behavior for the same situation (see
// internal/k8sagent/agent.go's cache branch: "cache scope pod unavailable;
// skipping cache for step" -> reports Succeeded). Before this fix, the host
// hard-failed the step here via markFailed, diverging from k8s.
func TestExecuteRun_ScopedCache_ScopeUnavailable_WarnAndSkip(t *testing.T) {
	const agentID = "scoped-cache-unavailable-agent"
	const runID = "run-scoped-cache-unavailable"

	workDir := t.TempDir()

	var reportedStatus string
	finishCh := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		if req.Status == "Succeeded" || req.Status == "Failed" {
			reportedStatus = req.Status
		}
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

	rt := &failingCreateRT{}
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = rt

	resp := api.ClaimResponse{
		RunID:   runID,
		JobName: "test-scoped-cache-unavailable",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:      0,
				StageIndex: 0,
				Name:       "cache-step",
				ScopeID:    "scope:build",
				ScopeImage: "golang:1.22",
				Cache: &dsl.CacheStep{
					Key:  "k1",
					Path: "deps",
				},
			}},
		},
	}

	a.executeRun(context.Background(), resp, workDir)

	select {
	case status := <-finishCh:
		if status != "Succeeded" {
			t.Fatalf("expected run Succeeded despite unavailable scope (cache is warn+skip), got %s", status)
		}
	default:
		t.Fatal("FinishRun was not called")
	}
	if reportedStatus != "Succeeded" {
		t.Fatalf("expected the cache step itself to report Succeeded (warn+skip), got %q", reportedStatus)
	}
}

// TestExecuteRun_ScopedStep_RunsInScopeContainer verifies that a step carrying
// ScopeID executes via the scope container's Exec (scopeManager.exec), not the
// host RunStepCapture path. A marker file written by a real host command would
// prove the step ran on the host, which must not happen for a scoped step.
func TestExecuteRun_ScopedStep_RunsInScopeContainer(t *testing.T) {
	const agentID = "scoped-run-agent"
	const runID = "run-scoped-run"

	workDir := t.TempDir()
	markerPath := filepath.Join(workDir, "marker.txt")

	var reportedStatus string
	finishCh := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		if req.Status == "Succeeded" || req.Status == "Failed" {
			reportedStatus = req.Status
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
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

	rt := &fakeRT{}
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = rt

	resp := api.ClaimResponse{
		RunID:   runID,
		JobName: "test-scoped-run",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:      0,
				StageIndex: 0,
				Name:       "scoped-step",
				ScopeID:    "scope:build",
				ScopeImage: "golang:1.22",
				Run:        "echo ran > " + markerPath,
			}},
		},
	}

	a.executeRun(context.Background(), resp, workDir)

	select {
	case status := <-finishCh:
		if status != "Succeeded" {
			t.Fatalf("expected Succeeded, got %s", status)
		}
	default:
		t.Fatal("FinishRun was not called")
	}
	if reportedStatus != "Succeeded" {
		t.Fatalf("expected step Succeeded, got %q", reportedStatus)
	}
	if rt.created != 1 {
		t.Fatalf("expected the scope container to be created exactly once, got %d", rt.created)
	}
	if rt.lastExec != "echo ran > "+markerPath {
		t.Fatalf("expected the scope container's Exec to run the step script, got %q", rt.lastExec)
	}
	if rt.removed != 1 {
		t.Fatalf("expected the scope container to be torn down at claim end, got %d removals", rt.removed)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("marker file should not exist on host; scoped step must execute inside the scope container, not on the host (stat err: %v)", err)
	}
}

// concurrentRT is a concurrency-safe fake ContainerRuntime used by tests that
// drive multiple scoped steps through real goroutines (parallel: stages).
// All state is behind atomics/a mutex so it is race-detector-clean even when
// several steps' scope containers are provisioned/executed/torn down at once.
type concurrentRT struct {
	createCalls atomic.Int64
	removeCalls atomic.Int64

	mu        sync.Mutex
	execCalls []string // every script passed to Exec, across all containers
	nextID    int
}

func (c *concurrentRT) Name() string                       { return "concurrent" }
func (c *concurrentRT) Available() bool                    { return true }
func (c *concurrentRT) Pull(context.Context, string) error { return nil }
func (c *concurrentRT) Run(context.Context, crt.RunSpec, io.Writer, io.Writer) (int, error) {
	return 0, nil
}
func (c *concurrentRT) Create(context.Context, crt.CreateSpec) (crt.ContainerHandle, error) {
	c.createCalls.Add(1)
	c.mu.Lock()
	c.nextID++
	id := fmt.Sprintf("c%d", c.nextID)
	c.mu.Unlock()
	return crt.ContainerHandle{ID: id}, nil
}
func (c *concurrentRT) Exec(_ context.Context, _ crt.ContainerHandle, spec crt.ExecSpec, _, _ io.Writer) (int, error) {
	c.mu.Lock()
	c.execCalls = append(c.execCalls, spec.Script)
	c.mu.Unlock()
	return 0, nil
}
func (c *concurrentRT) CopyIn(context.Context, crt.ContainerHandle, string, string) error { return nil }
func (c *concurrentRT) CopyOut(context.Context, crt.ContainerHandle, string, string) error {
	return nil
}
func (c *concurrentRT) Remove(context.Context, crt.ContainerHandle) error {
	c.removeCalls.Add(1)
	return nil
}

func (c *concurrentRT) execScripts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.execCalls))
	copy(out, c.execCalls)
	return out
}

// TestExecuteRun_ParallelScopedSteps_ConcurrentProvisioning drives a claim
// with a parallel: group of scoped steps — some sharing a ScopeID+MatrixKey
// (must reuse one container) and some with distinct ScopeIDs (must each get
// their own) — through the real executeRun/RunPipeline/runParallel goroutine
// path used for parallel: stages. This is the regression test for Finding 1
// (data race in scope provisioning / getScopes lazy-init): run with -race,
// this must complete cleanly with no "concurrent map writes" panic and with
// exactly the expected number of Create calls.
func TestExecuteRun_ParallelScopedSteps_ConcurrentProvisioning(t *testing.T) {
	const agentID = "parallel-scoped-agent"
	const runID = "run-parallel-scoped"

	workDir := t.TempDir()

	var finishStatus atomic.Value
	finishCh := make(chan struct{}, 1)

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
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/1/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/2/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		finishStatus.Store(body.Status)
		select {
		case finishCh <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	rt := &concurrentRT{}
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = rt

	// Three parallel members: two share (ScopeID, MatrixKey) = ("scope:shared", "")
	// and must reuse one container; the third has a distinct ScopeID and must
	// get its own. All three race to call getScopes()/ensure() concurrently
	// via runParallel's goroutines.
	resp := api.ClaimResponse{
		RunID:   runID,
		JobName: "test-parallel-scoped",
		Stages: []api.ClaimStage{
			{Parallel: []api.ClaimStep{
				{Index: 0, StageIndex: 0, Name: "shared-a", ScopeID: "scope:shared", ScopeImage: "img:shared", Run: "echo a"},
				{Index: 1, StageIndex: 0, Name: "shared-b", ScopeID: "scope:shared", ScopeImage: "img:shared", Run: "echo b"},
				{Index: 2, StageIndex: 0, Name: "distinct", ScopeID: "scope:distinct", ScopeImage: "img:distinct", Run: "echo c"},
			}},
		},
	}

	a.executeRun(context.Background(), resp, workDir)

	select {
	case <-finishCh:
	default:
		t.Fatal("FinishRun was not called")
	}
	if got, _ := finishStatus.Load().(string); got != "Succeeded" {
		t.Fatalf("expected run Succeeded, got %q", got)
	}

	// Two distinct scope keys → two containers: one for "scope:shared"
	// (reused by both shared-a and shared-b), one for "scope:distinct".
	if got := rt.createCalls.Load(); got != 2 {
		t.Fatalf("expected exactly 2 Create calls (one per distinct scope key), got %d", got)
	}
	if got := rt.removeCalls.Load(); got != 2 {
		t.Fatalf("expected exactly 2 Remove calls at claim teardown, got %d", got)
	}
	scripts := rt.execScripts()
	if len(scripts) != 3 {
		t.Fatalf("expected 3 Exec calls (one per parallel step), got %d: %v", len(scripts), scripts)
	}
}

// TestExecuteRun_ScopedStep_PostHookRunsInScopeContainer verifies Finding 2:
// a scoped step's post: hook must execute inside that step's scope
// container (via scopeManager.exec), not on the host workspace via
// RunStepCapture. Regression signal: the post script writes a marker file
// using an absolute host path; if the post hook incorrectly ran on the host,
// the marker file would exist. Since concurrentRT.Exec doesn't touch the
// filesystem, the file must NOT exist — and the exec log must show both the
// step's Run script and the post step's Run script routed through the scope
// container's Exec.
func TestExecuteRun_ScopedStep_PostHookRunsInScopeContainer(t *testing.T) {
	const agentID = "scoped-post-agent"
	const runID = "run-scoped-post"

	workDir := t.TempDir()
	markerPath := filepath.Join(workDir, "post-marker.txt")

	var finished atomic.Bool
	finishCh := make(chan string, 1)

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
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		finished.Store(true)
		select {
		case finishCh <- body.Status:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	rt := &concurrentRT{}
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = rt

	postScript := "echo posted > " + markerPath
	resp := api.ClaimResponse{
		RunID:   runID,
		JobName: "test-scoped-post",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:      0,
				StageIndex: 0,
				Name:       "scoped-with-post",
				ScopeID:    "scope:build",
				ScopeImage: "golang:1.22",
				Run:        "echo main",
				Post:       &api.PostStep{Run: postScript},
			}},
		},
	}

	a.executeRun(context.Background(), resp, workDir)

	select {
	case status := <-finishCh:
		if status != "Succeeded" {
			t.Fatalf("expected Succeeded, got %s", status)
		}
	default:
		t.Fatal("FinishRun was not called")
	}
	if !finished.Load() {
		t.Fatal("run did not finish")
	}

	// Exactly one container: the step's own scope. Verify it was created
	// before teardown, and both the step body and the post script were
	// routed through this container's Exec (not through RunStepCapture).
	if got := rt.createCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 Create (the step's scope container), got %d", got)
	}
	if got := rt.removeCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 Remove at claim teardown, got %d", got)
	}
	scripts := rt.execScripts()
	if len(scripts) != 2 {
		t.Fatalf("expected 2 Exec calls (step body + post hook), got %d: %v", len(scripts), scripts)
	}
	if scripts[0] != "echo main" {
		t.Fatalf("expected first Exec to be the step body, got %q", scripts[0])
	}
	if scripts[1] != postScript {
		t.Fatalf("expected second Exec to be the post hook script (proving it ran in the scope container), got %q", scripts[1])
	}
	// The strongest signal: if the post hook had run on the host instead
	// (regression to RunStepCapture(hookCtx, cmd, ..., workDir)), this file
	// would exist. concurrentRT.Exec never touches the filesystem.
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("post-hook marker file should not exist on host; post: must execute inside the scope container (stat err: %v)", err)
	}
}

// TestGetScopes_ConcurrentLazyInit exercises the getScopes closure's
// nil-check-and-assign directly by racing many goroutines through
// executeRun's parallel: scoped-step path (the only way to reach getScopes
// from outside the package). This is a focused regression test for Finding 1:
// under -race, a racy lazy-init would either panic (concurrent map writes in
// a doubly-constructed scopeManager.open) or fail the count assertions below
// by constructing two independent scopeManagers that each Create their own
// container for what should be a single shared scope key.
func TestGetScopes_ConcurrentLazyInit(t *testing.T) {
	const agentID = "lazy-init-agent"
	const runID = "run-lazy-init"

	workDir := t.TempDir()
	finishCh := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	for i := 0; i < 20; i++ {
		mux.HandleFunc(fmt.Sprintf("POST /api/v1/agents/%s/runs/%s/steps/%d/logs/bulk", agentID, runID, i), func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	}
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

	rt := &concurrentRT{}
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = rt

	// 20 parallel members, all sharing one ScopeID/MatrixKey, so every one of
	// them must call the same claim's getScopes() at (nearly) the same time.
	const n = 20
	members := make([]api.ClaimStep, n)
	for i := 0; i < n; i++ {
		members[i] = api.ClaimStep{Index: i, StageIndex: 0, Name: fmt.Sprintf("member-%d", i), ScopeID: "scope:shared", ScopeImage: "img", Run: "echo hi"}
	}
	resp := api.ClaimResponse{
		RunID:   runID,
		JobName: "test-lazy-init",
		Stages:  []api.ClaimStage{{Parallel: members}},
	}

	a.executeRun(context.Background(), resp, workDir)

	select {
	case status := <-finishCh:
		if status != "Succeeded" {
			t.Fatalf("expected Succeeded, got %s", status)
		}
	default:
		t.Fatal("FinishRun was not called")
	}

	if got := rt.createCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 Create for a single shared scope key across %d concurrent getScopes() callers, got %d", n, got)
	}
	if got := rt.removeCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 Remove at claim teardown, got %d", got)
	}
}

// TestExecuteRun_ParallelPostHooks_ConcurrentAppendIsSafe is the regression
// test for the "unguarded concurrent append to the post-hook slice(s)"
// finding: makeStepRunner's closure appends to the claim-scoped hookStack
// (agent.go, guarded by postHooksMu) whenever a step with `post:` succeeds,
// and runParallel (pipeline.go) invokes that closure concurrently — one
// goroutine per parallel: member. Before the fix, two or more parallel
// members each carrying a post: hook would race on `append(hookStack, ...)`,
// which `go test -race` flags as a data race and which can also corrupt the
// backing array / silently drop an appended entry.
//
// This test drives a real parallel: stage with several members that each
// have a distinct post: hook (through the real executeRun/RunPipeline/
// runParallel path, not a synthetic unit test of the slice), then asserts
// every single post hook actually ran by checking for its marker file. Run
// under `go test -race` this both proves there is no data race left and — by
// checking every marker file, not just len(hookStack) — proves no entry was
// silently lost to the race.
func TestExecuteRun_ParallelPostHooks_ConcurrentAppendIsSafe(t *testing.T) {
	const agentID = "parallel-post-agent"
	const runID = "run-parallel-post"

	workDir := t.TempDir()

	finishCh := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	for i := 0; i < 8; i++ {
		mux.HandleFunc(fmt.Sprintf("POST /api/v1/agents/%s/runs/%s/steps/%d/logs/bulk", agentID, runID, i), func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	}
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

	// Several parallel members, each with its own post: hook writing a
	// distinct marker file. All run concurrently via runParallel, so all of
	// their hookStack appends race unless serialized by postHooksMu.
	const n = 8
	members := make([]api.ClaimStep, n)
	markerPaths := make([]string, n)
	for i := 0; i < n; i++ {
		markerPaths[i] = filepath.Join(workDir, fmt.Sprintf("post-marker-%d.txt", i))
		// Use forward slashes in the script itself: on Windows the script is
		// interpreted by git-bash (findShell), which chokes on raw backslash
		// Windows paths embedded in a command string (e.g. "\a" is read as an
		// escape) even though os.ReadFile below uses the native path fine.
		markerPathForScript := strings.ReplaceAll(markerPaths[i], "\\", "/")
		members[i] = api.ClaimStep{
			Index:      i,
			StageIndex: 0,
			Name:       fmt.Sprintf("member-%d", i),
			Run:        "echo main",
			Post:       &api.PostStep{Run: "echo posted > \"" + markerPathForScript + "\""},
		}
	}
	resp := api.ClaimResponse{
		RunID:   runID,
		JobName: "test-parallel-post",
		Stages:  []api.ClaimStage{{Parallel: members}},
	}

	a.executeRun(context.Background(), resp, workDir)

	select {
	case status := <-finishCh:
		if status != "Succeeded" {
			t.Fatalf("expected Succeeded, got %s", status)
		}
	default:
		t.Fatal("FinishRun was not called")
	}

	// The real assertion: every single parallel member's post: hook ran.
	// Under the old unguarded append, some entries could be lost to the race
	// (backing-array corruption / lost writes), which would show up here as
	// a missing marker file for one or more members.
	for i, mp := range markerPaths {
		got, err := os.ReadFile(mp)
		if err != nil {
			t.Fatalf("member %d: post hook marker file missing (post hook did not run or was lost to a race): %v", i, err)
		}
		if string(got) != "posted\n" {
			t.Fatalf("member %d: unexpected marker content %q", i, string(got))
		}
	}
}
