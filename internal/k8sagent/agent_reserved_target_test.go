package k8sagent

import (
	"context"
	"io"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func reservedTargetClaim(runID string, stages, finally []api.ClaimStage) api.ClaimResponse {
	return api.ClaimResponse{RunID: runID, JobName: "reserved", Stages: stages, Finally: finally}
}

func TestK8sExecuteRun_RefusesReservedStepTargets(t *testing.T) {
	bad := api.ClaimStep{Index: 0, StageIndex: 0, Name: "x", Container: "unified-artifact", Run: "env"}
	ok := api.ClaimStep{Index: 1, StageIndex: 1, Name: "ok", Run: "echo hi"}
	cases := map[string]func(runID string) api.ClaimResponse{
		"plain step": func(runID string) api.ClaimResponse {
			return reservedTargetClaim(runID, []api.ClaimStage{{Step: &bad}}, nil)
		},
		"parallel step": func(runID string) api.ClaimResponse {
			return reservedTargetClaim(runID, []api.ClaimStage{{Parallel: []api.ClaimStep{ok, bad}}}, nil)
		},
		"finally step": func(runID string) api.ClaimResponse {
			return reservedTargetClaim(runID, []api.ClaimStage{{Step: &ok}}, []api.ClaimStage{{Step: &bad}})
		},
	}
	i := 0
	for name, mk := range cases {
		i++
		t.Run(name, func(t *testing.T) {
			agentID, runID := "k8s-reserved-"+string(rune('a'+i)), "run-reserved-"+string(rune('a'+i))
			s := newPolicyRunServer(t, agentID, runID)
			pm := &fakePM{}
			a := &K8sAgent{cfg: Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04"}, client: s.client, pm: pm, exec: &fakeExec{}}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			a.executeRun(ctx, mk(runID))

			require.Equal(t, api.RunFailed, s.waitFinish(t))
			lines := s.logLines()
			require.Len(t, lines, 1)
			require.Contains(t, lines[0], `k8s: step "x" targets container "unified-artifact"`)
			require.Nil(t, pm.created, "a refused run must never create a Pod")
		})
	}
}

func TestForbiddenTargetStep_AllowsOrdinaryContainers(t *testing.T) {
	c := reservedTargetClaim("r", []api.ClaimStage{
		{Step: &api.ClaimStep{Name: "a", Container: "job"}},
		{Parallel: []api.ClaimStep{{Name: "b", Container: "tools"}, {Name: "c"}}},
	}, nil)
	_, _, found := forbiddenTargetStep(c)
	require.False(t, found)
}

func TestK8sBackend_RefusesReservedContainers(t *testing.T) {
	for _, ctr := range []string{"unified-artifact", "ucd-shim"} {
		t.Run(ctr, func(t *testing.T) {
			ex := &fakeExec{}
			b := newK8sBackend(&K8sAgent{exec: ex}, "run-1", "job", "pod-default", "/workspace", nil, metav1.Time{})

			code, err := b.RunNamedContainer(context.Background(), api.ClaimStep{Name: "s"}, ctr, "env", nil, io.Discard, io.Discard)
			require.Error(t, err)
			require.Equal(t, -1, code)
			require.Contains(t, err.Error(), "reserved container")

			err = b.RunPostHook(context.Background(), agentlib.ScopeHandle{}, ctr, "env", nil, nil, io.Discard, io.Discard)
			require.Error(t, err)
			require.Contains(t, err.Error(), "reserved container")

			require.Empty(t, ex.gotPod, "no exec may reach the executor")
			require.Empty(t, ex.gotContainer)
		})
	}
}
