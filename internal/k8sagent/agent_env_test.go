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

// envCapturePM is a minimal podManager fake that always serves a fixed pod
// name and never fails, so the test can focus on the exec-time env.
type envCapturePM struct{}

func (envCapturePM) CreatePod(_ context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	out := pod.DeepCopy()
	if out.Name == "" {
		out.Name = "run-pod-env-1"
	}
	return out, nil
}
func (envCapturePM) WaitForPodRunning(_ context.Context, _ string) error { return nil }
func (envCapturePM) DeletePod(_ context.Context, _ string) error         { return nil }
func (envCapturePM) ListPods(_ context.Context, _ string) (*corev1.PodList, error) {
	return &corev1.PodList{}, nil
}

// envCaptureExec is a stepExecutor fake that records the env passed to each
// ExecStep call (once ExecStep grows an env parameter). Until then it records
// nothing extra; the RED test asserts on the recorded env and fails to
// compile/pass against the pre-fix signature.
type envCaptureExec struct {
	mu    sync.Mutex
	calls []envExecCall
}

type envExecCall struct {
	pod, container, script string
	env                    []string
}

func (f *envCaptureExec) ExecStep(_ context.Context, podName, container, script string, env []string, stdout, _ io.Writer) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, envExecCall{pod: podName, container: container, script: script, env: env})
	f.mu.Unlock()
	_, _ = stdout.Write([]byte("ok\n"))
	return 0, nil
}
func (f *envCaptureExec) ExecStepArgv(_ context.Context, _, _ string, _ []string, _, _ io.Writer) (int, error) {
	return 0, nil
}

// TestExecuteRun_DefaultStep_EnvInjected is a regression test for TODO #40: a
// step's env: map (and the UNIFIED_AGENT_OS convenience var) must reach the
// pod exec for the default/main-pod path. The extraEnv construction
// (agentOSForStep + step.Env) is done once by the shared orchestration loop
// (agentlib.RunClaim, internal/agent/orchestrator.go) and passed down to
// ExecBackend.RunDefault, so this is the same code path the host agent uses.
func TestExecuteRun_DefaultStep_EnvInjected(t *testing.T) {
	const agentID = "k8s-env-1"
	const runID = "run-env-1"

	finishCh := make(chan api.RunStatus, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, _ *http.Request) {
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

	pm := envCapturePM{}
	ex := &envCaptureExec{}
	agentClient := agentlib.NewClient(srv.URL, "tok")

	cfg := Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04"}
	a := &K8sAgent{cfg: cfg, client: agentClient, pm: pm, exec: ex}

	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "env-injection",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, StageIndex: 0, Name: "build",
				Run: "echo hi",
				Env: map[string]string{"FOO": "bar"},
			}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.executeRun(ctx, claim)

	select {
	case status := <-finishCh:
		require.Equal(t, api.RunSucceeded, status)
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun not called")
	}

	ex.mu.Lock()
	defer ex.mu.Unlock()
	require.Len(t, ex.calls, 1)
	call := ex.calls[0]
	assert.Contains(t, call.env, "FOO=bar", "step env: must reach the pod exec")
	assert.Contains(t, call.env, "UNIFIED_AGENT_OS=linux", "pod-exec steps always run on linux")
}
