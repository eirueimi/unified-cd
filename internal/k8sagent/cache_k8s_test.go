//go:build k8s

package k8sagent

// TestK8sAgent_CacheRoundTrip_Integration verifies that a cache step works
// end-to-end via the unified-artifact sidecar in a real Kubernetes cluster,
// using the direct-S3 model: the sidecar execs the unified-sidecar binary
// against an S3-compatible bucket (no controller involvement in the transfer
// itself). Cache restore happens at step time (best-effort); cache save is
// deferred until the end of the run (matching the standard agent's cache
// semantics).
//
// Because save is deferred to end-of-run, a single run cannot observe its
// own save being restored. This test therefore runs the job TWICE with the
// same cache key: the first run seeds the cache path and lets the deferred
// save upload it to S3; the second run (a fresh pod) restores from that key
// and asserts the file is back.
//
// Prerequisites:
//   - A reachable Kubernetes cluster (via default kubeconfig).
//   - The pod image (ubuntu:22.04) and the sidecar image
//     (ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest) must be pullable
//     from within the cluster. If the sidecar image is not yet available on the
//     cluster, pre-load it (e.g. `kind load docker-image …`) before running.
//   - A Kubernetes Secret providing UNIFIED_S3_ENDPOINT/UNIFIED_S3_BUCKET/
//     UNIFIED_S3_KEY/UNIFIED_S3_SECRET/UNIFIED_S3_USE_SSL/UNIFIED_S3_REGION
//     must already exist in the test namespace; its name is passed via the
//     UNIFIED_TEST_S3_SECRET env var. Without it, this test skips (it cannot
//     fabricate S3 credentials for a real cluster run).
//
// For CI without a cluster, skip this file by not passing -tags k8s.
// This test is intentionally skipped when -short is set.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestK8sAgent_CacheRoundTrip_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cache round-trip integration test in -short mode")
	}

	s3Secret := os.Getenv("UNIFIED_TEST_S3_SECRET")
	if s3Secret == "" {
		t.Skip("UNIFIED_TEST_S3_SECRET not set; skipping direct-S3 cache round-trip (needs a pre-created Secret with UNIFIED_S3_* env in the test namespace)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client, restCfg := newTestKubeClient(t)
	ns := newTestNamespace(t, client)

	const agentID = "k8s-cache-e2e"
	cacheKey := uniqueRunID("cache-e2e-key")

	var mu sync.Mutex
	stepStatuses := map[string]string{}
	finishStatuses := map[string]api.RunStatus{}
	finishCh := make(chan struct{}, 2)

	// The mock controller in this test only needs to serve run/step/log
	// bookkeeping endpoints — cache bytes now flow directly between the
	// sidecar and S3, never through the controller.
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
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/", func(w http.ResponseWriter, r *http.Request) {
		// The finish handler below is registered with a more specific pattern
		// per run, but ServeMux prefers the longest match automatically, so
		// this catch-all only serves the log-flush path.
		w.WriteHeader(http.StatusNoContent)
	})

	registerFinish := func(runID string) {
		mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
			mu.Lock()
			finishStatuses[runID] = api.RunStatus(body["status"])
			mu.Unlock()
			select {
			case finishCh <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/outputs", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	}

	runID1 := uniqueRunID("cache-e2e-seed")
	runID2 := uniqueRunID("cache-e2e-restore")
	registerFinish(runID1)
	registerFinish(runID2)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pm := NewPodManager(client, ns, testImage)
	exec := NewExecutor(client, restCfg, ns)
	pool := NewPodPool(client, ns, pm)
	agentClient := agentlib.NewClient(srv.URL, "tok")

	cfg := Config{
		AgentID:             agentID,
		Namespace:           ns,
		PodImage:            testImage,
		SidecarImage:        "ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest",
		Server:              srv.URL,
		Token:               "tok",
		SidecarS3SecretName: s3Secret,
	}
	a := NewK8sAgent(cfg, agentClient, pm, exec, pool)

	waitForFinish := func(runID string) api.RunStatus {
		select {
		case <-finishCh:
		case <-time.After(5 * time.Minute):
			t.Fatalf("FinishRun not called within 5 minutes for run %s", runID)
		}
		mu.Lock()
		defer mu.Unlock()
		status, ok := finishStatuses[runID]
		if !ok {
			t.Fatalf("FinishRun not recorded for run %s", runID)
		}
		return status
	}

	// Run 1 (seed): write a file into the cache path, then a cache step
	// restores (miss, since nothing is cached yet under this key) and
	// registers a deferred save. The save uploads the cache path to S3 after
	// the main stages complete.
	seedClaim := api.ClaimResponse{
		RunID:   runID1,
		JobName: "cache-round-trip-seed",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "write-file",
				Run: "mkdir -p /workspace/cachedir && echo 'hello-cache' > /workspace/cachedir/f.txt"}},
			{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "cache-seed",
				Cache: &dsl.CacheStep{Path: "cachedir", Key: cacheKey, TTLDays: 1}}},
		},
	}
	a.executeRun(ctx, seedClaim)
	require.Equal(t, api.RunSucceeded, waitForFinish(runID1), "cache seed run should succeed; step statuses: %v", stepStatuses)

	mu.Lock()
	assert.Equal(t, "Succeeded", stepStatuses["write-file"])
	assert.Equal(t, "Succeeded", stepStatuses["cache-seed"])
	mu.Unlock()

	// Run 2 (restore): a fresh pod, no pre-existing cachedir. The cache step
	// restores from the key saved by run 1; a final run step asserts the
	// restored file's content is correct.
	restoreClaim := api.ClaimResponse{
		RunID:   runID2,
		JobName: "cache-round-trip-restore",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "cache-restore",
				Cache: &dsl.CacheStep{Path: "cachedir", Key: cacheKey, TTLDays: 1}}},
			{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "verify",
				Run: "cat /workspace/cachedir/f.txt | grep -q 'hello-cache'"}},
		},
	}
	a.executeRun(ctx, restoreClaim)
	require.Equal(t, api.RunSucceeded, waitForFinish(runID2), "cache restore run should succeed; step statuses: %v", stepStatuses)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "Succeeded", stepStatuses["cache-restore"])
	assert.Equal(t, "Succeeded", stepStatuses["verify"], fmt.Sprintf("expected restored cache file to be present and match; step statuses: %v", stepStatuses))
}
