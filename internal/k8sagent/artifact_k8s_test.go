//go:build k8s

package k8sagent

// TestK8sAgent_ArtifactRoundTrip_Integration verifies that uploadArtifact and
// downloadArtifact steps work end-to-end via the unified-artifact sidecar in a
// real Kubernetes cluster, using controller-issued URLs: the sidecar execs the
// unified-sidecar binary, which asks the (mock) controller where to transfer.
//
// Prerequisites:
//   - A reachable Kubernetes cluster (via default kubeconfig), e.g. kind.
//   - The pod image (ubuntu:22.04) must be pullable from within the cluster.
//   - A sidecar image built from this tree (docker/artifact-sidecar.Dockerfile, or the
//     repository's Makefile target) and loaded into the cluster (e.g.
//     `kind load docker-image ...`); its name is passed via
//     UNIFIED_TEST_SIDECAR_IMAGE. The published image may predate the
//     controller-issued-URL protocol, so there is no default.
//   - UNIFIED_TEST_HOST_ADDR: an address of this machine that the cluster's
//     Pods can reach (for kind, the Docker network gateway). The test serves a
//     mock controller there: it answers presign requests with mode
//     "controller" and streams archives through its own routes, so no object
//     store and no Secret are needed.
//   Without these variables the test skips.
//
// For CI without a cluster, skip this file by not passing -tags k8s.
// This test is intentionally skipped when -short is set.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
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

	sidecarImage, hostAddr := sidecarTestEnvOrSkip(t)
	shimImage := testShimImageOrSkip(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client, restCfg := newTestKubeClient(t)
	ns := newTestNamespace(t, client)

	const agentID = "k8s-artifact-e2e"
	runID := uniqueRunID("artifact-e2e")

	var mu sync.Mutex
	stepStatuses := map[string]string{}
	finishCh := make(chan api.RunStatus, 1)

	// The mock controller serves run/step/log bookkeeping plus the object
	// routes the sidecar uses (mode "controller": archives stream through it).
	mux := http.NewServeMux()
	newMockObjects().register(mux)
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

	srvURL := serveReachable(t, mux, hostAddr)

	pm := NewPodManager(client, ns, testImage)
	exec := NewExecutor(client, restCfg, ns)
	pool := NewPodPool(client, ns, pm)
	agentClient := agentlib.NewClient(srvURL, "tok")

	cfg := Config{
		AgentID:      agentID,
		Namespace:    ns,
		PodImage:     testImage,
		ShimImage:    shimImage,
		SidecarImage: sidecarImage,
		Server:       srvURL,
	}
	a := NewK8sAgent(cfg, agentClient, pm, exec, pool)

	// The round-trip stages:
	//  1. run:  write a file into the workspace.
	//  2. uploadArtifact: exec `unified-sidecar artifact upload` (through the mock controller).
	//  3. run:  delete the file to prove it is gone.
	//  4. downloadArtifact: exec `unified-sidecar artifact download` (through the mock controller).
	//  5. run:  cat the restored file and assert the content is correct.
	//
	// Steps 2 and 4 call the real sidecar binary, which reads its projected
	// token and the controller URL from the Pod and requests the transfer from
	// the mock controller above.
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

// sidecarTestEnvOrSkip returns the sidecar image and the host address the
// round-trip tests need, or skips when they are not provided.
func sidecarTestEnvOrSkip(t *testing.T) (sidecarImage, hostAddr string) {
	t.Helper()
	sidecarImage = os.Getenv("UNIFIED_TEST_SIDECAR_IMAGE")
	hostAddr = os.Getenv("UNIFIED_TEST_HOST_ADDR")
	if sidecarImage == "" || hostAddr == "" {
		t.Skip("UNIFIED_TEST_SIDECAR_IMAGE and UNIFIED_TEST_HOST_ADDR not set; skipping the sidecar round-trip (needs a sidecar image built from this tree loaded into the cluster, and a host address the Pods can reach)")
	}
	return sidecarImage, hostAddr
}

// serveReachable serves h on all interfaces and returns its URL as seen from
// the cluster's Pods (hostAddr is this machine's address there).
func serveReachable(t *testing.T, h http.Handler, hostAddr string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	return "http://" + net.JoinHostPort(hostAddr, port)
}

// mockObjects is the object side of a mock controller: every presign request
// is answered with mode "controller", and archives are streamed through the
// run-scoped routes into memory. It does not check the pod token.
type mockObjects struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMockObjects() *mockObjects { return &mockObjects{data: map[string][]byte{}} }

func (m *mockObjects) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/runs/{run}/objects/presign", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.PresignResponse{Mode: api.ObjectModeController})
	})
	mux.HandleFunc("PUT /api/v1/runs/{run}/objects/artifacts/{name}", func(w http.ResponseWriter, r *http.Request) {
		m.put(w, r, "artifact/"+r.PathValue("run")+"/"+r.PathValue("name"))
	})
	mux.HandleFunc("GET /api/v1/runs/{run}/objects/artifacts/{name}", func(w http.ResponseWriter, r *http.Request) {
		src := r.PathValue("run")
		if s := r.URL.Query().Get("sourceRunId"); s != "" {
			src = s
		}
		m.get(w, "artifact/"+src+"/"+r.PathValue("name"), api.ObjectErrNotFound, "")
	})
	mux.HandleFunc("PUT /api/v1/runs/{run}/objects/caches", func(w http.ResponseWriter, r *http.Request) {
		m.put(w, r, "cache/"+r.URL.Query().Get("key"))
	})
	mux.HandleFunc("GET /api/v1/runs/{run}/objects/caches", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		m.get(w, "cache/"+key, api.ObjectErrCacheMiss, key)
	})
}

func (m *mockObjects) put(w http.ResponseWriter, r *http.Request, key string) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.data[key] = b
	m.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (m *mockObjects) get(w http.ResponseWriter, key, missCode, matchedKey string) {
	m.mu.Lock()
	b, ok := m.data[key]
	m.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(api.ObjectError{Error: "not found", Code: missCode})
		return
	}
	if matchedKey != "" {
		w.Header().Set("X-Unified-Cache-Key", url.PathEscape(matchedKey))
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	_, _ = w.Write(b)
}
