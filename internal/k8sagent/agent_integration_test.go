//go:build k8s

package k8sagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestK8sAgent_ExecuteRun_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, restCfg := newTestKubeClient(t)
	shimImage := testShimImageOrSkip(t)
	ns := newTestNamespace(t, client)

	const agentID = "k8s-e2e"
	const runID = "run-e2e"

	var mu sync.Mutex
	var logLines []string
	finishCh := make(chan api.RunStatus, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	// Registered for parity with every other agent endpoint's test harness,
	// but neither stream posts here any more: stdout and stderr both ship via
	// their own LogPusher onto the bulk endpoint below.
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, r *http.Request) {
		var req api.LogAppendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			mu.Lock()
			logLines = append(logLines, req.Line)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// stdout and stderr bulk from each stream's own LogPusher (auto-flush
	// timer and finish's final Flush)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		if err := json.NewDecoder(r.Body).Decode(&reqs); err == nil {
			mu.Lock()
			for _, req := range reqs {
				if req.Line != "" {
					logLines = append(logLines, req.Line)
				}
			}
			mu.Unlock()
		}
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

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pm := NewPodManager(client, ns, testImage)
	exec := NewExecutor(client, restCfg, ns)
	pool := NewPodPool(client, ns, pm)
	agentClient := agentlib.NewClient(srv.URL, "tok")

	cfg := Config{
		AgentID:   agentID,
		Namespace: ns,
		PodImage:  testImage,
		ShimImage: shimImage,
	}
	a := NewK8sAgent(cfg, agentClient, pm, exec, pool)

	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "e2e-test",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, Name: "hello", Run: "echo hello-from-k8s-agent"}},
		},
	}

	a.executeRun(ctx, claim)

	select {
	case status := <-finishCh:
		require.Equal(t, api.RunSucceeded, status, "run should succeed")
	case <-time.After(30 * time.Second):
		t.Fatal("FinishRun not called within 30 seconds")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.NotEmpty(t, logLines, "expected at least one stdout log line")
	found := false
	for _, line := range logLines {
		if strings.Contains(line, "hello-from-k8s-agent") {
			found = true
			break
		}
	}
	assert.True(t, found, "expected log line containing 'hello-from-k8s-agent', got: %v", logLines)
}

func TestK8sAgent_ExecuteRun_StepFailure_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, restCfg := newTestKubeClient(t)
	shimImage := testShimImageOrSkip(t)
	ns := newTestNamespace(t, client)

	const agentID = "k8s-e2e-fail"
	const runID = "run-e2e-fail"

	finishCh := make(chan api.RunStatus, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", func(w http.ResponseWriter, _ *http.Request) {
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

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pm := NewPodManager(client, ns, testImage)
	exec := NewExecutor(client, restCfg, ns)
	pool := NewPodPool(client, ns, pm)
	agentClient := agentlib.NewClient(srv.URL, "tok")

	cfg := Config{AgentID: agentID, Namespace: ns, PodImage: testImage, ShimImage: shimImage}
	a := NewK8sAgent(cfg, agentClient, pm, exec, pool)

	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "e2e-fail-test",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, Name: "fail-step", Run: "exit 42"}},
		},
	}

	a.executeRun(ctx, claim)

	select {
	case status := <-finishCh:
		assert.Equal(t, api.RunFailed, status, "run should fail when step exits non-zero")
	case <-time.After(30 * time.Second):
		t.Fatal("FinishRun not called within 30 seconds")
	}
}

// concurrentBarrierScriptMinRuntime is a floor every member's script spends
// running before it can possibly exit, regardless of how fast the barrier
// resolves. It exists so the test's shrunk stderrAutoFlushInterval (see
// TestK8sAgent_ExecuteRun_ParallelMembersRunConcurrently_Integration) is
// structurally guaranteed to tick at least once *during* the step rather
// than only firing via the unconditional final Flush that StepLogWriters'
// finish closure does at step end (internal/k8sagent/backend.go). Without
// this floor, a fast-resolving barrier (three execs starting within
// milliseconds of each other) could let the whole script finish before the
// auto-flush ticker ever fires, and the test would go green having never
// exercised the mechanism it exists to verify.
const concurrentBarrierScriptMinRuntime = 1 * time.Second

// concurrentBarrierScript returns the shell body one parallel-group member
// (of numConcurrentMembers, indices 0..numConcurrentMembers-1) runs for
// TestK8sAgent_ExecuteRun_ParallelMembersRunConcurrently_Integration.
//
// Each member emits two stdout and two stderr lines, sleeps for
// concurrentBarrierScriptMinRuntime (see its doc comment), drops a signal
// file named after its own index into the shared /workspace, then spins
// waiting for every other member's signal file to appear before emitting two
// more stdout and two more stderr lines and exiting 0.
//
// The wait is deliberately NOT a timing assertion (the design brief calls
// that out as flaky-by-construction): nothing here measures wall-clock
// overlap or finish order. Instead it is a rendezvous every member must
// clear before any of them can finish. If Task 2's ConcurrencyMode flip
// regressed and the k8s backend went back to running parallel members one at
// a time, member 0 would reach the wait loop while members 1 and 2 haven't
// started yet — their signal files would never appear, member 0 would
// exhaust the bounded retry loop below, and the step would fail loudly
// instead of the suite hanging. The 300*0.2s = 60s bound is unverified
// against real kind-cluster exec latency for three simultaneous `kubectl
// exec` sessions into one container (new ground for this suite), so it is
// generous on purpose: too small fails as a confusing flake that looks like
// a concurrency bug, too large only costs wall-clock on a test that is
// already the slowest thing in this file.
func concurrentBarrierScript(idx, numConcurrentMembers int) string {
	// allPresent is a "&&"-chain of "[ -f signal_dir/N ]" checks, one per
	// member (self included -- harmless, since this member's own signal file
	// is dropped just before the loop). "&&"/"!" are core shell syntax rather
	// than test(1) flags, unlike "-a"/"-o" (which POSIX marks obsolescent and
	// not every "[" implementation honors), so this stays portable across
	// whatever "[ ]" ucd-sh's interpreter provides.
	var allPresent strings.Builder
	for i := 0; i < numConcurrentMembers; i++ {
		if i > 0 {
			allPresent.WriteString(" && ")
		}
		fmt.Fprintf(&allPresent, `[ -f "$signal_dir/%d" ]`, i)
	}
	return fmt.Sprintf(`set -e
signal_dir=/workspace/concurrent-signals
mkdir -p "$signal_dir"
echo "member-%[1]d-stdout-1"
echo "member-%[1]d-stderr-1" >&2
echo "member-%[1]d-stdout-2"
echo "member-%[1]d-stderr-2" >&2
sleep %[3]s
: > "$signal_dir/%[1]d"
attempt=0
while ! ( %[2]s ); do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 300 ]; then
    echo "member-%[1]d: gave up waiting for the other members' signal files after 60s -- steps did not run concurrently" >&2
    exit 1
  fi
  sleep 0.2
done
echo "member-%[1]d-stdout-3"
echo "member-%[1]d-stderr-3" >&2
echo "member-%[1]d-stdout-4"
echo "member-%[1]d-stderr-4" >&2
`, idx, allPresent.String(), fmt.Sprintf("%.3f", concurrentBarrierScriptMinRuntime.Seconds()))
}

// TestK8sAgent_ExecuteRun_ParallelMembersRunConcurrently_Integration is the
// scenario the design brief calls for: a parallel: group of three steps
// executed against one real Pod, exercising machinery the unit tests and the
// parity case never touch — three concurrent execs into the same container,
// and three independent StepLogWriters auto-flush timers actually ticking
// under that concurrent load. See concurrentBarrierScript for why the
// concurrency proof is a deterministic rendezvous rather than a timing
// measurement, and the "does NOT cover" paragraph below for the one thing
// named in the design brief's motivation that this scenario deliberately
// leaves untouched.
//
// Assertions are limited to outcomes: every member's terminal status, the
// run's terminal status, and that every member's stdout/stderr lines
// actually arrived at the controller (proving the per-step log writers held
// up under concurrent load, auto-flush ticks included -- see
// stderrAutoFlushInterval below). Nothing here asserts ordering or measures
// elapsed time.
//
// What this does NOT cover: the sidecar log pump (k8sSidecarPump, started in
// k8sBackend.SetMasker). SetMasker builds that pump once per claim, before
// the step loop runs at all, and each user sidecar's own LogPusher writes
// under dsl.SidecarLogIndex -- a distinct index space from any step's index,
// so it shares no per-step state with concurrent parallel members. Step
// concurrency structurally cannot reach it; there is nothing here for a
// sidecar container to prove that a plain claim-level pump test wouldn't
// already cover. See design doc §6's outcome note for the full reasoning.
func TestK8sAgent_ExecuteRun_ParallelMembersRunConcurrently_Integration(t *testing.T) {
	// The auto-flush ticker (internal/k8sagent/agent.go's
	// stderrAutoFlushInterval, default 2s) competes with StepLogWriters'
	// finish closure, which does an unconditional final Flush at step end.
	// At the default interval, a fast-resolving barrier could let every
	// member's script finish in under 2s, so all lines would ship via the
	// final flush and the ticker would never fire -- the test would pass
	// without ever touching the mechanism it exists to verify. Shrinking the
	// interval (same idiom as secrets_masking_k8s_test.go), combined with
	// concurrentBarrierScriptMinRuntime's floor on each script's runtime,
	// guarantees the ticker fires multiple times mid-step, under real
	// concurrent load, before any member can reach its final flush.
	prevInterval := stderrAutoFlushInterval
	stderrAutoFlushInterval = 200 * time.Millisecond
	t.Cleanup(func() { stderrAutoFlushInterval = prevInterval })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, restCfg := newTestKubeClient(t)
	shimImage := testShimImageOrSkip(t)
	ns := newTestNamespace(t, client)

	const agentID = "k8s-concurrent-e2e"
	const runID = "run-concurrent-e2e"
	const numConcurrentMembers = 3

	var mu sync.Mutex
	stepStatuses := map[string]string{}
	logLines := map[int][]string{} // stepIndex -> lines (stdout + stderr, interleaved as received)
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
	// Registered for parity with every other agent endpoint's test harness,
	// but neither stream posts here any more (both ship via the bulk endpoint
	// below).
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, r *http.Request) {
		var req api.LogAppendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			mu.Lock()
			logLines[req.StepIndex] = append(logLines[req.StepIndex], req.Line)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// stdout and stderr bulk from each member's own LogPushers (auto-flush
	// timer and final flush) -- {idx} is a path variable because three
	// members flush to three different per-step URLs concurrently.
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		idx, _ := strconv.Atoi(r.PathValue("idx"))
		var reqs []api.LogAppendRequest
		if err := json.NewDecoder(r.Body).Decode(&reqs); err == nil {
			mu.Lock()
			for _, req := range reqs {
				if req.Line != "" {
					logLines[idx] = append(logLines[idx], req.Line)
				}
			}
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/outputs", func(w http.ResponseWriter, _ *http.Request) {
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

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pm := NewPodManager(client, ns, testImage)
	exec := NewExecutor(client, restCfg, ns)
	pool := NewPodPool(client, ns, pm)
	agentClient := agentlib.NewClient(srv.URL, "tok")

	cfg := Config{
		AgentID:   agentID,
		Namespace: ns,
		PodImage:  testImage,
		ShimImage: shimImage,
	}
	a := NewK8sAgent(cfg, agentClient, pm, exec, pool)

	members := make([]api.ClaimStep, numConcurrentMembers)
	for i := 0; i < numConcurrentMembers; i++ {
		members[i] = api.ClaimStep{
			Index:      i,
			StageIndex: 0,
			Name:       fmt.Sprintf("member-%d", i),
			Run:        concurrentBarrierScript(i, numConcurrentMembers),
		}
	}
	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "concurrent-parallel-test",
		Stages: []api.ClaimStage{
			{Parallel: members},
		},
	}

	a.executeRun(ctx, claim)

	select {
	case status := <-finishCh:
		mu.Lock()
		statusesCopy := fmt.Sprintf("%v", stepStatuses)
		mu.Unlock()
		require.Equal(t, api.RunSucceeded, status, "run should succeed when all parallel members succeed; step statuses: %s", statusesCopy)
	case <-time.After(3 * time.Minute):
		t.Fatal("FinishRun not called within 3 minutes")
	}

	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < numConcurrentMembers; i++ {
		name := fmt.Sprintf("member-%d", i)
		assert.Equal(t, "Succeeded", stepStatuses[name], "member %d (%s) status; all statuses: %v", i, name, stepStatuses)

		lines := logLines[i]
		assert.NotEmpty(t, lines, "member %d: expected log lines, got none", i)
		for _, want := range []string{
			fmt.Sprintf("member-%d-stdout-1", i),
			fmt.Sprintf("member-%d-stdout-2", i),
			fmt.Sprintf("member-%d-stdout-3", i),
			fmt.Sprintf("member-%d-stdout-4", i),
			fmt.Sprintf("member-%d-stderr-1", i),
			fmt.Sprintf("member-%d-stderr-2", i),
			fmt.Sprintf("member-%d-stderr-3", i),
			fmt.Sprintf("member-%d-stderr-4", i),
		} {
			assert.Contains(t, lines, want, "member %d: expected log line %q among: %v", i, want, lines)
		}
	}
}
