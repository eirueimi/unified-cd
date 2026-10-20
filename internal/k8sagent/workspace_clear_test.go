package k8sagent

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"k8s.io/client-go/kubernetes/fake"
)

// clearExec records every script it is asked to run and answers each with a
// fixed exit code and error.
type clearExec struct {
	mu      sync.Mutex
	scripts []string
	exit    int
	err     error
}

func (f *clearExec) ExecStep(_ context.Context, _, _, script string, _ []string, _ []string, _, _ io.Writer) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, script)
	return f.exit, f.err
}
func (f *clearExec) ExecStepArgv(_ context.Context, _, _ string, _ []string, _, _ io.Writer) (int, error) {
	return 0, nil
}

func claimWithStep(tpl *dsl.PodTemplate) api.ClaimResponse {
	return api.ClaimResponse{RunID: "r1", PodTemplate: tpl, Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "s", Run: "echo ok"}},
	}}
}

// A reused pod outlives its runs, so its workspace is cleared before every run
// whether or not the template asked for it: the default workspace is
// ephemeral, as on a fresh pod's emptyDir.
func TestClearReusedWorkspace_ClearsWithoutCleanWorkspaceSet(t *testing.T) {
	ex := &clearExec{}
	a := &K8sAgent{exec: ex}
	c := claimWithStep(&dsl.PodTemplate{Reuse: true})
	if err := a.clearReusedWorkspace(context.Background(), "pod-1", c, "/workspace", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if len(ex.scripts) != 1 {
		t.Fatalf("want exactly one clearing exec, got %v", ex.scripts)
	}
	s := ex.scripts[0]
	if !strings.Contains(s, "rm -rf") || !strings.Contains(s, "/workspace") {
		t.Fatalf("script does not clear /workspace: %q", s)
	}
	// Dotfiles (a checkout's .git) are part of the previous run's workspace.
	if !strings.Contains(s, ".[!.]*") || !strings.Contains(s, "..?*") {
		t.Fatalf("script must also remove dot-prefixed entries: %q", s)
	}
}

func TestClearReusedWorkspace_UsesTheConfiguredMountPath(t *testing.T) {
	ex := &clearExec{}
	a := &K8sAgent{exec: ex}
	c := claimWithStep(&dsl.PodTemplate{Reuse: true, Workspace: &dsl.WorkspaceConfig{MountPath: "/work/dir"}})
	if err := a.clearReusedWorkspace(context.Background(), "pod-1", c, "/work/dir", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !strings.Contains(ex.scripts[0], "/work/dir") {
		t.Fatalf("script ignores the configured mount path: %q", ex.scripts[0])
	}
}

// A workspace that could not be emptied must not be run in: it would leak the
// previous run's files into this one.
func TestClearReusedWorkspace_FailureIsReported(t *testing.T) {
	for name, ex := range map[string]*clearExec{
		"exec error": {err: errors.New("pod gone")},
		"exit code":  {exit: 1},
	} {
		a := &K8sAgent{exec: ex}
		if err := a.clearReusedWorkspace(context.Background(), "pod-1", claimWithStep(&dsl.PodTemplate{Reuse: true}), "/workspace", nil); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}
}

// executeRun wires the clearing in for every pooled claim, and a pod whose
// workspace could not be cleared is deleted, never returned to the idle pool.
func TestExecuteRun_PooledPodClearFailureFailsRunAndDeletesPod(t *testing.T) {
	prevInitial, prevMax := agentlib.RetryInitialWait, agentlib.RetryMaxWait
	agentlib.RetryInitialWait, agentlib.RetryMaxWait = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { agentlib.RetryInitialWait, agentlib.RetryMaxWait = prevInitial, prevMax })

	var status atomic.Value
	var finishCalled atomic.Int32
	srv := controllerForWait(t, &status, &finishCalled)

	const ns = "ns"
	kube := fake.NewSimpleClientset()
	pool := NewPodPool(kube, ns, NewPodManager(kube, ns, "img"))
	pm := &fakePM{}
	ex := &clearExec{exit: 1}
	a := &K8sAgent{
		cfg:    Config{AgentID: "k8s-1", Namespace: ns, PodImage: "img", ShimImage: "shim"},
		client: agentlib.NewClient(srv.URL, "tok"),
		pm:     pm,
		exec:   ex,
		pool:   pool,
	}
	c := claimWithStep(&dsl.PodTemplate{
		Reuse: true, // CleanWorkspace deliberately unset
		Spec: map[string]any{
			"containers": []any{map[string]any{"name": "job", "image": "img"}},
		},
	})

	a.executeRun(context.Background(), c)

	if len(ex.scripts) != 1 || !strings.Contains(ex.scripts[0], "rm -rf") {
		t.Fatalf("a pooled claim must clear the workspace before its first step even without cleanWorkspace; execs: %v", ex.scripts)
	}
	if finishCalled.Load() == 0 {
		t.Fatal("a workspace that cannot be cleared must fail the run")
	}
	if len(pm.deleted) == 0 {
		t.Fatal("the pod must be deleted, not released with a dirty workspace")
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for _, pods := range pool.pods {
		if len(pods) != 0 {
			t.Fatalf("a dirty pod must not return to the idle pool; got %d idle", len(pods))
		}
	}
}
