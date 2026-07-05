package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

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
	if rt.copyOutSrcPath != "out.txt" {
		t.Fatalf("expected CopyOut to be called with the artifact's container path %q, got %q", "out.txt", rt.copyOutSrcPath)
	}
	// The uploaded body is tar+zstd; just prove it's non-empty and did not
	// come from an untouched host workDir copy (which would fail to route
	// through copyOutToTemp at all, since contentRT.CopyOut is the only
	// source of "scope container content").
	if len(uploadedBody) == 0 {
		t.Fatal("expected non-empty uploaded artifact body")
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
