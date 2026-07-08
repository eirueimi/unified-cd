//go:build integration

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// claimIntegrationHarness is a minimal fake controller recording, per step
// index, the stdout lines shipped via AppendLogBulk, plus the run's final
// status. Modeled after named_container_integration_test.go's harness (the
// file this test supersedes) and agent_isolated_test.go's isolatedHarness.
type claimIntegrationHarness struct {
	mu         sync.Mutex
	stepStdout map[int][]string
	finishCh   chan string
}

func newClaimIntegrationHarness() *claimIntegrationHarness {
	return &claimIntegrationHarness{stepStdout: map[int][]string{}, finishCh: make(chan string, 1)}
}

func newClaimIntegrationServer(t *testing.T, agentID string, h *claimIntegrationHarness) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	// Covers both the per-step bulk log endpoint and the stepIndex==-1 system
	// log endpoint (failRun's AppendLogBulk uses the same path shape).
	mux.HandleFunc("/api/v1/agents/"+agentID+"/runs/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/logs/bulk") {
			parts := strings.Split(r.URL.Path, "/")
			stepIndex := 0
			for i, p := range parts {
				if p == "steps" && i+1 < len(parts) {
					if idx, err := strconv.Atoi(parts[i+1]); err == nil {
						stepIndex = idx
					}
				}
			}
			var entries []api.LogAppendRequest
			if err := json.NewDecoder(r.Body).Decode(&entries); err == nil {
				h.mu.Lock()
				for _, e := range entries {
					if e.Stream == "stdout" {
						h.stepStdout[stepIndex] = append(h.stepStdout[stepIndex], e.Line)
					}
				}
				h.mu.Unlock()
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/outputs") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/finish") {
			var body struct {
				Status string `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			select {
			case h.finishCh <- body.Status:
			default:
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestClaimPod_Integration_SidecarLocalhostAndWorkspace is a real-Docker/
// Podman round-trip proving the claim pod's three isolation guarantees at
// once: (1) every container shares the workspace bind mount (a default step
// writes a file, a "web" container step serves it, a later default step reads
// it back over HTTP); (2) every container shares the pause netns, so the
// "web" container's busybox httpd on port 12080 is reachable from the default
// ("job") container via plain localhost, with nothing published; (3) default
// steps in an isolated claim report UNIFIED_AGENT_OS=linux. Skips when no
// container runtime is available. This is the claim-pod counterpart to the
// deleted named_container_integration_test.go (superseded: that test's
// premise, a lazily-created named container with no shared netns, died with
// the claim-pod refactor).
func TestClaimPod_Integration_SidecarLocalhostAndWorkspace(t *testing.T) {
	if _, err := crt.Detect(""); err != nil {
		t.Skipf("no container runtime available, skipping: %v", err)
	}

	const agentID = "claim-pod-integration-agent"
	const runID = "run-claim-pod-integration"

	h := newClaimIntegrationHarness()
	srv := newClaimIntegrationServer(t, agentID, h)

	a := &Agent{
		ID:          agentID,
		Client:      NewClient(srv.URL, "tok"),
		PauseImage:  "busybox:1.36",
		RunnerImage: "busybox:1.36",
	}

	claim := api.ClaimResponse{
		Native:  false,
		RunID:   runID,
		JobName: "test-claim-pod-integration",
		PodTemplate: &dsl.PodTemplate{Spec: map[string]any{
			"containers": []any{
				map[string]any{"name": "web", "image": "busybox:1.36"},
			},
		}},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, StageIndex: 0, Name: "write-workspace-file",
				Run: "echo hello > /workspace/hello.txt",
			}},
			{Step: &api.ClaimStep{
				Index: 1, StageIndex: 1, Name: "serve-workspace",
				Container: "web",
				Run:       "httpd -p 12080 -h /workspace",
			}},
			{Step: &api.ClaimStep{
				Index: 2, StageIndex: 2, Name: "fetch-via-localhost",
				Run: "wget -qO- http://localhost:12080/hello.txt | grep hello " +
					"&& echo \"UNIFIED_AGENT_OS=$UNIFIED_AGENT_OS\"",
			}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	select {
	case status := <-h.finishCh:
		require.Equal(t, "Succeeded", status, "run should finish Succeeded")
	default:
		t.Fatal("FinishRun was not called")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	step3Stdout := strings.Join(h.stepStdout[2], "\n")
	assert.Contains(t, step3Stdout, "hello",
		"step 3 must read back the file the default step wrote and the web container served, got: %q", step3Stdout)
	assert.Contains(t, step3Stdout, "UNIFIED_AGENT_OS=linux",
		"default steps in an isolated claim must report linux, got: %q", step3Stdout)
}

// TestClaimPod_Integration_ConcurrentClaimsNoPortCollision proves claim pods
// give each claim its own network namespace: two concurrent claims of the
// same job shape each bind port 12080 in their own "job" container, with no
// published ports and no shared netns between them, so neither collides with
// the other and both succeed.
func TestClaimPod_Integration_ConcurrentClaimsNoPortCollision(t *testing.T) {
	if _, err := crt.Detect(""); err != nil {
		t.Skipf("no container runtime available, skipping: %v", err)
	}

	const agentID = "claim-pod-concurrent-agent"

	run := func(t *testing.T, runID string) string {
		t.Helper()
		h := newClaimIntegrationHarness()
		srv := newClaimIntegrationServer(t, agentID, h)

		a := &Agent{
			ID:          agentID,
			Client:      NewClient(srv.URL, "tok"),
			PauseImage:  "busybox:1.36",
			RunnerImage: "busybox:1.36",
		}

		claim := api.ClaimResponse{
			Native:  false,
			RunID:   runID,
			JobName: "test-claim-pod-concurrent",
			Stages: []api.ClaimStage{
				{Step: &api.ClaimStep{
					Index: 0, StageIndex: 0, Name: "serve-and-wait",
					Run: "mkdir -p /workspace/www && echo ok > /workspace/www/ok.txt && " +
						"httpd -p 12080 -h /workspace/www && " +
						"wget -qO- http://localhost:12080/ok.txt | grep ok",
				}},
			},
		}

		a.executeRun(context.Background(), claim, t.TempDir())

		select {
		case status := <-h.finishCh:
			return status
		default:
			t.Fatal("FinishRun was not called")
			return ""
		}
	}

	var wg sync.WaitGroup
	statuses := make([]string, 2)
	runIDs := []string{"run-claim-pod-concurrent-a", "run-claim-pod-concurrent-b"}
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = run(t, runIDs[i])
		}(i)
	}
	wg.Wait()

	for i, status := range statuses {
		assert.Equal(t, "Succeeded", status, "claim %d (%s) should succeed: isolated netns means both can bind port 12080", i, runIDs[i])
	}
}
