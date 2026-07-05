package k8sagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// scopeLifecyclePM is a fakePM variant that assigns a distinct generated name
// per CreatePod call (the run pod vs. the scope pod must be distinguishable),
// and records every created/deleted pod name so the test can assert the scope
// pod (and only the scope pod) is created once and deleted at claim end.
type scopeLifecyclePM struct {
	mu      sync.Mutex
	seq     int
	created []*corev1.Pod
	deleted []string
	waitErr error
}

func (f *scopeLifecyclePM) CreatePod(_ context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	out := pod.DeepCopy()
	if out.Name == "" {
		// GenerateName-style pod (scope pod / throwaway pod): assign a unique name.
		out.Name = pod.GenerateName + "generated" + string(rune('0'+f.seq))
	}
	f.created = append(f.created, out)
	return out, nil
}
func (f *scopeLifecyclePM) WaitForPodRunning(_ context.Context, _ string) error {
	return f.waitErr
}
func (f *scopeLifecyclePM) DeletePod(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	return nil
}
func (f *scopeLifecyclePM) ListPods(_ context.Context, _ string) (*corev1.PodList, error) {
	return &corev1.PodList{}, nil
}

// scopeLifecycleExec is a stepExecutor fake that records every ExecStep/
// ExecStepArgv call's (podName, container) pair (and argv, for the sidecar
// calls) so the test can assert the run step exec'd into the "step" container
// of the scope pod, and the upload argv/sidecar targeted the scope pod too.
type scopeLifecycleExec struct {
	mu        sync.Mutex
	execCalls []struct{ pod, container, script string }
	argvCalls []struct {
		pod, container string
		argv           []string
	}
}

func (f *scopeLifecycleExec) ExecStep(_ context.Context, podName, container, script string, stdout, _ io.Writer) (int, error) {
	f.mu.Lock()
	f.execCalls = append(f.execCalls, struct{ pod, container, script string }{podName, container, script})
	f.mu.Unlock()
	_, _ = stdout.Write([]byte("ok\n"))
	return 0, nil
}
func (f *scopeLifecycleExec) ExecStepArgv(_ context.Context, podName, container string, argv []string, _, _ io.Writer) (int, error) {
	f.mu.Lock()
	f.argvCalls = append(f.argvCalls, struct {
		pod, container string
		argv           []string
	}{podName, container, argv})
	f.mu.Unlock()
	return 0, nil
}

// TestExecuteRun_ScopedStep_UsesScopePodAndCleansUp drives a full claim (one
// scoped run step + one scoped upload-artifact step) through executeRun with
// fakes, asserting:
//   - exactly one scope pod is created (buildScopePod invoked once, cached by
//     scopeKey across both scoped steps),
//   - the run step's exec targeted the scope pod's "step" container,
//   - the upload's sidecar exec targeted the scope pod (not the run pod) with
//     an argv path under scopeMountPath,
//   - the scope pod is deleted by claim end.
func TestExecuteRun_ScopedStep_UsesScopePodAndCleansUp(t *testing.T) {
	const agentID = "k8s-scope-1"
	const runID = "run-scope-1"

	var mu sync.Mutex
	statuses := map[string]string{}
	finishCh := make(chan api.RunStatus, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.StepName != "" {
			mu.Lock()
			statuses[req.StepName] = req.Status
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		select {
		case finishCh <- api.RunStatus(body["status"]):
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/outputs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pm := &scopeLifecyclePM{}
	ex := &scopeLifecycleExec{}
	agentClient := agentlib.NewClient(srv.URL, "tok")

	cfg := Config{
		AgentID:      agentID,
		Namespace:    "ci",
		PodImage:     "ubuntu:22.04",
		SidecarImage: "sidecar:1",
	}
	a := &K8sAgent{cfg: cfg, client: agentClient, pm: pm, exec: ex}

	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "scope-lifecycle",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, StageIndex: 0, Name: "build",
				ScopeID: "scope:build", ScopeImage: "golang:1.22",
				Run: "go build ./...",
			}},
			{Step: &api.ClaimStep{
				Index: 1, StageIndex: 1, Name: "upload",
				ScopeID: "scope:build", ScopeImage: "golang:1.22",
				UploadArtifact: &api.UploadArtifactStep{Name: "bin", Path: "out/bin"},
			}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.executeRun(ctx, claim)

	select {
	case status := <-finishCh:
		require.Equal(t, api.RunSucceeded, status, "run should succeed; step statuses: %v", statuses)
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun not called")
	}

	mu.Lock()
	assert.Equal(t, "Succeeded", statuses["build"])
	assert.Equal(t, "Succeeded", statuses["upload"])
	mu.Unlock()

	// Exactly one scope pod was created: 1 run pod + 1 scope pod = 2 total
	// CreatePod calls, and only one carries the scopeId annotation.
	pm.mu.Lock()
	var scopePodNames []string
	for _, p := range pm.created {
		if p.Annotations["unified-cd/scopeId"] == "scope:build" {
			scopePodNames = append(scopePodNames, p.Name)
		}
	}
	deletedCopy := append([]string(nil), pm.deleted...)
	pm.mu.Unlock()
	require.Len(t, scopePodNames, 1, "expected exactly one scope pod created (cached across both scoped steps), got: %+v", pm.created)
	scopePodName := scopePodNames[0]

	// The run step exec'd into the scope pod's "step" container, not the run pod.
	ex.mu.Lock()
	defer ex.mu.Unlock()
	require.NotEmpty(t, ex.execCalls)
	found := false
	for _, c := range ex.execCalls {
		if c.pod == scopePodName {
			assert.Equal(t, "step", c.container, "scoped run step must exec into the scope pod's \"step\" container")
			assert.Equal(t, "go build ./...", c.script)
			found = true
		}
	}
	assert.True(t, found, "expected an ExecStep call targeting the scope pod %q, got: %+v", scopePodName, ex.execCalls)

	// The upload's sidecar exec targeted the scope pod with a scopeMountPath argv.
	uploadFound := false
	for _, c := range ex.argvCalls {
		if c.pod == scopePodName {
			uploadFound = true
			assert.Equal(t, artifactSidecarName, c.container)
			assert.Contains(t, c.argv, "/workspace/out/bin", "upload path must be under scopeMountPath")
		}
	}
	assert.True(t, uploadFound, "expected a sidecar argv call targeting the scope pod %q, got: %+v", scopePodName, ex.argvCalls)

	// The scope pod is deleted at claim end.
	assert.Contains(t, deletedCopy, scopePodName, "scope pod must be deleted at claim end")
}
