//go:build k8s

package k8sagent

// TestK8sAgent_ArtifactRoundTrip_Integration verifies that uploadArtifact and
// downloadArtifact steps work end-to-end via the unified-artifact sidecar in a
// real Kubernetes cluster.
//
// Prerequisites:
//   - A reachable Kubernetes cluster (via default kubeconfig).
//   - The pod image (ubuntu:22.04) and the sidecar image
//     (ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest) must be pullable
//     from within the cluster. If the sidecar image is not yet available on the
//     cluster, pre-load it (e.g. `kind load docker-image …`) before running.
//   - A running unified-cd controller reachable from within the pod at
//     $UNIFIED_SERVER with a valid $UNIFIED_AGENT_TOKEN; the test mock server
//     used here does NOT implement the artifact store, so the actual curl/tar
//     transfer will fail unless a real store is wired up.
//
// For CI without a cluster, skip this file by not passing -tags k8s.
// This test is intentionally skipped when -short is set.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestK8sAgent_ArtifactRoundTrip_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping artifact round-trip integration test in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client, restCfg := newTestKubeClient(t)
	ns := newTestNamespace(t, client)

	const agentID = "k8s-artifact-e2e"
	runID := uniqueRunID("artifact-e2e")

	var mu sync.Mutex
	stepStatuses := map[string]string{}
	finishCh := make(chan api.RunStatus, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.StepName != "" {
			mu.Lock()
			stepStatuses[req.StepName] = req.Status
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	// Bulk stderr from LogPusher.Flush — accept any step index
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishCh <- api.RunStatus(body["status"]):
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// SetStepOutputs / SetRunOutputs
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/outputs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pm := NewPodManager(client, ns, testImage)
	exec := NewExecutor(client, restCfg, ns)
	pool := NewPodPool(client, ns, pm)
	agentClient := agentlib.NewClient(srv.URL, "tok")

	cfg := Config{
		AgentID:      agentID,
		Namespace:    ns,
		PodImage:     testImage,
		SidecarImage: "ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest",
		Server:       srv.URL,
		Token:        "tok",
	}
	a := NewK8sAgent(cfg, agentClient, pm, exec, pool)

	// The round-trip stages:
	//  1. run:  write a file into the workspace.
	//  2. uploadArtifact: tar|zstd|curl PUT the workspace directory.
	//  3. run:  delete the file to prove it is gone.
	//  4. downloadArtifact: curl|zstd|tar restore into a subdirectory.
	//  5. run:  cat the restored file and assert the content is correct.
	//
	// NOTE: steps 2 and 4 call the real sidecar, which needs $UNIFIED_SERVER and
	// $UNIFIED_AGENT_TOKEN to be set in the sidecar container. The mock server
	// above does NOT implement the artifact object store, so steps 2/4 will fail
	// unless you point cfg.Server at a real unified-cd controller.  The test
	// asserts Succeeded for all steps — it will fail gracefully if the store is
	// unavailable, producing a clear "Failed" status for the artifact steps.
	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "artifact-round-trip",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "write-file",
				Run: "echo 'hello-artifact' > /workspace/f.txt"}},
			{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "upload",
				UploadArtifact: &api.UploadArtifactStep{Name: "testartifact", Path: "."}}},
			{Step: &api.ClaimStep{Index: 2, StageIndex: 2, Name: "remove-file",
				Run: "rm -f /workspace/f.txt"}},
			{Step: &api.ClaimStep{Index: 3, StageIndex: 3, Name: "download",
				DownloadArtifact: &api.DownloadArtifactStep{Name: "testartifact", DestDir: "restored"}}},
			{Step: &api.ClaimStep{Index: 4, StageIndex: 4, Name: "verify",
				Run: "cat /workspace/restored/f.txt | grep -q 'hello-artifact'"}},
		},
	}

	a.executeRun(ctx, claim)

	select {
	case status := <-finishCh:
		require.Equal(t, api.RunSucceeded, status, "artifact round-trip run should succeed; step statuses: %v", stepStatuses)
	case <-time.After(5 * time.Minute):
		t.Fatal("FinishRun not called within 5 minutes")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "Succeeded", stepStatuses["write-file"])
	assert.Equal(t, "Succeeded", stepStatuses["upload"])
	assert.Equal(t, "Succeeded", stepStatuses["remove-file"])
	assert.Equal(t, "Succeeded", stepStatuses["download"])
	assert.Equal(t, "Succeeded", stepStatuses["verify"])
}
