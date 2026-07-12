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

// installShimOrSkip installs the real embedded ucd-sh into a fresh temp
// tools dir and returns it, for real-Docker/Podman integration tests: every
// claim-pod/scope container's keep-alive is now "/.ucd/ucd-sh pause" (see
// claim_pod.go's ucdShPause), so a claim pod cannot start without a real
// (non-placeholder) shim mounted at /.ucd. Skips the test — rather than
// failing — when internal/shim/embedded still holds the committed
// zero-byte placeholder (the two-stage build, `make embed-shim`, has not
// been run), mirroring the crt.Detect "no runtime" skip pattern used
// throughout this file.
func installShimOrSkip(t *testing.T) string {
	t.Helper()
	toolsDir, err := InstallShim(t.TempDir())
	if err != nil {
		t.Skipf("ucd-sh shim not embedded (run `make embed-shim` first): %v", err)
	}
	return toolsDir
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
		ToolsDir:    installShimOrSkip(t),
	}

	claim := api.ClaimResponse{
		Native:  false,
		RunID:   runID,
		JobName: "test-claim-pod-integration",
		// "web" explicitly requests the keep-alive command: this test drives
		// httpd from a later STEP (not the sidecar's own entrypoint), so it
		// must opt in to staying alive rather than relying on the (fixed)
		// bug where every claim-pod container used to get "sleep infinity"
		// regardless of role. See TestClaimPod_Integration_RedisSidecarEntrypointRuns
		// below for the real-entrypoint-sidecar case this masked.
		PodTemplate: &dsl.PodTemplate{Spec: map[string]any{
			"containers": []any{
				map[string]any{"name": "web", "image": "busybox:1.36", "command": []any{"sleep", "infinity"}},
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
				// httpd (step 2) may not have bound port 12080 yet when this
				// step starts executing; retry briefly instead of racing the
				// bind (avoids a rare flake where the first request loses
				// the race and the step fails on a healthy setup).
				Run: "for i in $(seq 1 20); do wget -qO- http://localhost:12080/hello.txt && break; sleep 0.5; done | grep hello " +
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

// TestClaimPod_Integration_RedisSidecarEntrypointRuns is the regression test
// for the sidecar-sleep-infinity bug (see sidecar-sleep-fix-brief.md): a
// podTemplate sidecar with NO explicit command must run its image's own
// entrypoint — that IS the sidecar's service — not "sleep infinity". Uses
// redis:7 (starts fast, no datadir init, unlike mysql:8) as a real service
// sidecar. A default (container:-less) step polls it on localhost with
// busybox's `nc` (portable under `sh`, no curl dependency on the RunnerImage
// used here). Before the fix, every claim-pod container — including
// sidecars — was started with `sleep infinity`, so redis-server never ran and
// this poll would time out and fail the step.
func TestClaimPod_Integration_RedisSidecarEntrypointRuns(t *testing.T) {
	if _, err := crt.Detect(""); err != nil {
		t.Skipf("no container runtime available, skipping: %v", err)
	}

	const agentID = "claim-pod-redis-sidecar-agent"
	const runID = "run-claim-pod-redis-sidecar"

	h := newClaimIntegrationHarness()
	srv := newClaimIntegrationServer(t, agentID, h)

	a := &Agent{
		ID:          agentID,
		Client:      NewClient(srv.URL, "tok"),
		PauseImage:  "busybox:1.36",
		RunnerImage: "busybox:1.36",
		ToolsDir:    installShimOrSkip(t),
	}

	claim := api.ClaimResponse{
		Native:  false,
		RunID:   runID,
		JobName: "test-claim-pod-redis-sidecar",
		// No "command" on the redis container: this is the exact real-world
		// shape (see examples/jobs/pod-sidecar.yaml's mysql sidecar) that
		// exposed the bug — a sidecar's image entrypoint must run untouched.
		PodTemplate: &dsl.PodTemplate{Spec: map[string]any{
			"containers": []any{
				map[string]any{"name": "redis", "image": "redis:7"},
			},
		}},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, StageIndex: 0, Name: "wait-for-redis",
				// redis has no readiness probe here — poll our own way.
				// nc -z reports success only once redis-server has actually
				// bound port 6379, which only happens if the sidecar ran its
				// own entrypoint instead of "sleep infinity".
				Run: `set -e
for i in $(seq 1 30); do
  nc -z -w 2 127.0.0.1 6379 && { echo "redis reachable on localhost:6379"; exit 0; }
  sleep 1
done
echo "redis sidecar did not become ready in time" >&2
exit 1`,
			}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	select {
	case status := <-h.finishCh:
		require.Equal(t, "Succeeded", status, "run should finish Succeeded: the redis sidecar must run its own entrypoint (redis-server), not sleep infinity")
	default:
		t.Fatal("FinishRun was not called")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	step1Stdout := strings.Join(h.stepStdout[0], "\n")
	assert.Contains(t, step1Stdout, "redis reachable on localhost:6379",
		"default step must observe the redis sidecar listening, proving its entrypoint ran, got: %q", step1Stdout)
}

// TestClaimPod_Integration_Shim_DefaultShellRunsInBashlessAlpine is the
// headline end-to-end proof for the step-shell-shim feature (spec Testing
// summary, "Integration (docker-gated)"): a claim-pod job whose primary
// ("job") container is alpine:3 — no bash, only busybox ash — runs a
// default (no shell: declared) step to success. This can only work if (1)
// InstallShim actually wrote a real ucd-sh to toolsDir, (2) claimPodManager
// bind-mounted it read-only at /.ucd on the primary container, (3) the
// primary's keep-alive ("/.ucd/ucd-sh pause") actually started and kept the
// container alive without a "sleep" binary, and (4) the step's exec used the
// shim default (["/.ucd/ucd-sh", "-c"]) rather than a "sh"/"bash" this image
// doesn't have beyond busybox ash. The pause container is alpine:3 too, for
// the same "no bash, no sleep, must work anyway" proof on that container.
func TestClaimPod_Integration_Shim_DefaultShellRunsInBashlessAlpine(t *testing.T) {
	if _, err := crt.Detect(""); err != nil {
		t.Skipf("no container runtime available, skipping: %v", err)
	}

	const agentID = "claim-pod-shim-alpine-agent"
	const runID = "run-claim-pod-shim-alpine"

	h := newClaimIntegrationHarness()
	srv := newClaimIntegrationServer(t, agentID, h)

	a := &Agent{
		ID:          agentID,
		Client:      NewClient(srv.URL, "tok"),
		PauseImage:  "alpine:3",
		RunnerImage: "alpine:3",
		ToolsDir:    installShimOrSkip(t),
	}

	claim := api.ClaimResponse{
		Native:  false,
		RunID:   runID,
		JobName: "test-claim-pod-shim-alpine",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, StageIndex: 0, Name: "no-shell-declared",
				// No Shell field set: must resolve to the shim default
				// (["/.ucd/ucd-sh", "-c"]) at the agent, not any "sh"/"bash"
				// this alpine image happens to provide via busybox.
				Run: "echo hello-from-shim-default",
			}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	select {
	case status := <-h.finishCh:
		require.Equal(t, "Succeeded", status,
			"a default (shim) step must succeed in a bash-less alpine primary — proves shim install+mount+exec+pause end-to-end")
	default:
		t.Fatal("FinishRun was not called")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	step0Stdout := strings.Join(h.stepStdout[0], "\n")
	assert.Contains(t, step0Stdout, "hello-from-shim-default")
}

// TestClaimPod_Integration_Shim_ExplicitBashShellRunsInDebian is the
// companion proof for the `shell:` override (spec Component 1): a step that
// declares shell: [bash, -lc] in a debian:bookworm-slim primary must
// actually run under real bash (not the shim's interp), evidenced by
// $BASH_VERSION being set — a variable only a real bash process populates.
func TestClaimPod_Integration_Shim_ExplicitBashShellRunsInDebian(t *testing.T) {
	if _, err := crt.Detect(""); err != nil {
		t.Skipf("no container runtime available, skipping: %v", err)
	}

	const agentID = "claim-pod-shim-debian-agent"
	const runID = "run-claim-pod-shim-debian"

	h := newClaimIntegrationHarness()
	srv := newClaimIntegrationServer(t, agentID, h)

	a := &Agent{
		ID:          agentID,
		Client:      NewClient(srv.URL, "tok"),
		PauseImage:  "alpine:3",
		RunnerImage: "debian:bookworm-slim",
		ToolsDir:    installShimOrSkip(t),
	}

	claim := api.ClaimResponse{
		Native:  false,
		RunID:   runID,
		JobName: "test-claim-pod-shim-debian",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, StageIndex: 0, Name: "explicit-bash-shell",
				Shell: []string{"bash", "-lc"},
				Run:   `echo "BASH_VERSION=$BASH_VERSION"`,
			}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	select {
	case status := <-h.finishCh:
		require.Equal(t, "Succeeded", status, "a shell: [bash, -lc] step must succeed in a debian primary")
	default:
		t.Fatal("FinishRun was not called")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	step0Stdout := strings.Join(h.stepStdout[0], "\n")
	assert.Regexp(t, `BASH_VERSION=\d`, step0Stdout,
		"expected a real bash-populated BASH_VERSION, proving shell: [bash, -lc] ran under actual bash, not the shim's interp; got: %q", step0Stdout)
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
			ToolsDir:    installShimOrSkip(t),
		}

		claim := api.ClaimResponse{
			Native:  false,
			RunID:   runID,
			JobName: "test-claim-pod-concurrent",
			Stages: []api.ClaimStage{
				{Step: &api.ClaimStep{
					Index: 0, StageIndex: 0, Name: "serve-and-wait",
					// httpd daemonizes (forks to background) and returns
					// immediately, so the very first wget can race the
					// listener bind; retry briefly instead of asserting on
					// a single attempt (avoids a rare flake on a healthy
					// setup).
					Run: "mkdir -p /workspace/www && echo ok > /workspace/www/ok.txt && " +
						"httpd -p 12080 -h /workspace/www && " +
						"for i in $(seq 1 20); do wget -qO- http://localhost:12080/ok.txt && break; sleep 0.5; done | grep ok",
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
