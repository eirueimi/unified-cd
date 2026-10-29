package k8sagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/require"
)

// policyRunServer serves the endpoints executeRun touches when it fails a run
// at claim time, and captures the step -1 log lines and the finish status.
type policyRunServer struct {
	mu     sync.Mutex
	lines  []string
	finish chan api.RunStatus
	client *agentlib.Client
}

func newPolicyRunServer(t *testing.T, agentID, runID string) *policyRunServer {
	t.Helper()
	s := &policyRunServer{finish: make(chan api.RunStatus, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/-1/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var body []api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		for _, l := range body {
			s.lines = append(s.lines, l.Line)
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		select {
		case s.finish <- api.RunStatus(body["status"]):
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s.client = agentlib.NewClient(srv.URL, "tok")
	return s
}

func (s *policyRunServer) logLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func (s *policyRunServer) waitFinish(t *testing.T) api.RunStatus {
	t.Helper()
	select {
	case st := <-s.finish:
		return st
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun not called")
		return ""
	}
}

func privilegedClaim(runID string) api.ClaimResponse {
	return api.ClaimResponse{
		RunID:   runID,
		JobName: "policy",
		PodTemplate: inlineJob(map[string]any{
			"securityContext": map[string]any{"privileged": true},
		}, nil),
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "build", Run: "echo hi"}},
		},
	}
}

func requirePolicyDenial(t *testing.T, s *policyRunServer) {
	t.Helper()
	require.Equal(t, api.RunFailed, s.waitFinish(t))
	lines := s.logLines()
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "k8s: job pod policy:")
	require.Contains(t, lines[0], "allowPrivileged")
}

func TestK8sExecuteRun_PodPolicyDeniesBeforeAnyPod(t *testing.T) {
	const agentID, runID = "k8s-policy-1", "run-policy-1"
	s := newPolicyRunServer(t, agentID, runID)
	pm := &fakePM{}
	a := &K8sAgent{cfg: Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04"}, client: s.client, pm: pm, exec: &fakeExec{}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.executeRun(ctx, privilegedClaim(runID))

	requirePolicyDenial(t, s)
	require.Nil(t, pm.created, "a refused run must never create a Pod")
}

func TestK8sExecuteRun_PodPolicyDeniesReuseBeforePool(t *testing.T) {
	const agentID, runID = "k8s-policy-2", "run-policy-2"
	s := newPolicyRunServer(t, agentID, runID)
	pm := &fakePM{}
	// pool is nil: reaching the pool would dereference it, so passing proves
	// the policy check runs first.
	a := &K8sAgent{cfg: Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04"}, client: s.client, pm: pm, exec: &fakeExec{}, pool: nil}

	claim := privilegedClaim(runID)
	claim.PodTemplate.Reuse = true

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NotPanics(t, func() { a.executeRun(ctx, claim) })

	requirePolicyDenial(t, s)
	require.Nil(t, pm.created)
}

func TestK8sExecuteRun_PodPolicyDeniesBeforeVolumeLocks(t *testing.T) {
	const agentID, runID = "k8s-policy-3", "run-policy-3"
	s := newPolicyRunServer(t, agentID, runID)
	pm := &fakePM{}
	cfg := Config{
		AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04",
		AgentName: "agent-a",
		Volumes:   []config.VolumeConfig{{Name: "go-cache", Mode: "exclusive"}},
	}
	// No kube client: taking the volume lock would fail differently, so the
	// policy error must win.
	a := &K8sAgent{cfg: cfg, client: s.client, pm: pm, exec: &fakeExec{}}

	claim := privilegedClaim(runID)
	claim.Volumes = []dsl.VolumeMount{{Name: dsl.StringOrSlice{"go-cache"}, MountPath: "cache"}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.executeRun(ctx, claim)

	requirePolicyDenial(t, s)
	require.Nil(t, pm.created)
}

func TestK8sExecuteRun_PodPolicyAllowsWithOptIn(t *testing.T) {
	const agentID, runID = "k8s-policy-4", "run-policy-4"
	s := newPolicyRunServer(t, agentID, runID)
	// The Pod never becomes ready, which ends the run right after CreatePod
	// without needing the exec stack.
	pm := &fakePM{waitErr: errors.New("stop after create")}
	cfg := Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04"}
	cfg.JobPodPolicy.AllowPrivileged = true
	a := &K8sAgent{cfg: cfg, client: s.client, pm: pm, exec: &fakeExec{}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.executeRun(ctx, privilegedClaim(runID))

	require.Equal(t, api.RunFailed, s.waitFinish(t))
	require.NotNil(t, pm.created, "with allowPrivileged the run must reach CreatePod")
	for _, l := range s.logLines() {
		require.NotContains(t, l, "job pod policy")
	}
}

// TestJobPodPolicy_UndecodableOrUnknownTemplateStillNeverBuildsAPod pins the
// division of labour: the policy returns nil for pod content it cannot decode
// and for an agent template name that does not exist, relying on BuildPod to
// fail the same input. Together the two never create a pod.
func TestJobPodPolicy_UndecodableOrUnknownTemplateStillNeverBuildsAPod(t *testing.T) {
	agentTmpls := map[string]AgentPodTemplate{
		"known": {Spec: map[string]any{"containers": []any{map[string]any{"name": "job", "image": "alpine:3.20"}}}},
	}
	cases := map[string]*dsl.PodTemplate{
		"env value is a number": inlineJob(map[string]any{
			"env": []any{map[string]any{"name": "A", "value": 1}},
		}, nil),
		"container is not an object": {Spec: map[string]any{"containers": []any{"not-an-object"}}},
		"unknown template name":      {Name: "missing"},
	}
	for name, tmpl := range cases {
		t.Run(name, func(t *testing.T) {
			policy := JobPodPolicy{}
			require.NoError(t, policy.CheckJobPodTemplate(agentTmpls, tmpl))
			pod, err := BuildPod("run-1", "ci", agentTmpls, tmpl, "ubuntu:22.04", SidecarSpec{}, "", nil)
			require.Error(t, err)
			require.Nil(t, pod)
		})
	}
}
