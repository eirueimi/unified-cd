package k8sagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/require"
)

// TestK8sExecuteRun_NativeClaimFailsFast verifies that the k8s agent rejects
// a native: true claim immediately: native jobs are host-only (they need the
// bare-process/claim-pod host stack, not a k8s Pod), so the k8s agent must
// fail the run without ever creating a Pod, rather than trying to schedule
// one it can never satisfy correctly.
func TestK8sExecuteRun_NativeClaimFailsFast(t *testing.T) {
	const agentID = "k8s-native-1"
	const runID = "run-native-1"

	finishCh := make(chan api.RunStatus, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/-1/logs/bulk", func(w http.ResponseWriter, _ *http.Request) {
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

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pm := &fakePM{}
	ex := &fakeExec{}
	agentClient := agentlib.NewClient(srv.URL, "tok")

	cfg := Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04"}
	a := &K8sAgent{cfg: cfg, client: agentClient, pm: pm, exec: ex}

	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "native-rejection",
		Native:  true,
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, StageIndex: 0, Name: "build",
				Run: "echo hi",
			}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.executeRun(ctx, claim)

	select {
	case status := <-finishCh:
		require.Equal(t, api.RunFailed, status)
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun not called")
	}

	require.Nil(t, pm.created, "no Pod should ever be created for a native claim")
}
