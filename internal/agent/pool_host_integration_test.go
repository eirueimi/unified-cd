//go:build integration

package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

const poolBusybox = "docker.io/library/busybox:1.36"

// poolLogServer is a fake controller that records every shipped log line (both
// streams) per step index and the run's final status.
type poolLogServer struct {
	mu     sync.Mutex
	lines  map[int][]string
	finish chan string
}

func newPoolLogServer(t *testing.T, agentID string) (*poolLogServer, *httptest.Server) {
	t.Helper()
	h := &poolLogServer{lines: map[int][]string{}, finish: make(chan string, 4)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agents/"+agentID+"/runs/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/logs/bulk"):
			parts := strings.Split(r.URL.Path, "/")
			idx := 0
			for i, p := range parts {
				if p == "steps" && i+1 < len(parts) {
					if n, err := strconv.Atoi(parts[i+1]); err == nil {
						idx = n
					}
				}
			}
			var entries []api.LogAppendRequest
			if json.NewDecoder(r.Body).Decode(&entries) == nil {
				h.mu.Lock()
				for _, e := range entries {
					h.lines[idx] = append(h.lines[idx], e.Line)
				}
				h.mu.Unlock()
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/finish"):
			var body struct {
				Status string `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			h.finish <- body.Status
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/outputs"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, srv
}

func (h *poolLogServer) take(idx int) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.lines[idx]
	h.lines = map[int][]string{}
	return out
}

// TestClaimPod_PoolReuse_SidecarLogsStartAfterPreviousRun runs two claims on one
// pooled pod against a real runtime. The sidecar writes to stdout, then ticks to
// stderr (so its last line before the pod is released is a stderr line) only
// while a step has marked the workspace; the marker is gone once the workspace
// is emptied, so the pod is silent while idle and the last tick before the
// second run is known exactly. The second run must reuse the pod and see its
// sidecar's first line as the very next tick, with nothing replayed.
func TestClaimPod_PoolReuse_SidecarLogsStartAfterPreviousRun(t *testing.T) {
	if _, err := crt.Detect(testRuntimePref()); err != nil {
		skipOrFailInCI(t, "no container runtime available (%s=%q): %v", containerRuntimeEnv, testRuntimePref(), err)
	}
	const agentID = "pool-reuse-integration-agent"
	h, srv := newPoolLogServer(t, agentID)
	a := &Agent{
		RuntimePref: testRuntimePref(), ID: agentID, Client: NewClient(srv.URL, "tok"),
		PauseImage: poolBusybox, RunnerImage: poolBusybox, ToolsDir: installShimOrSkip(t),
		PoolMaxIdle: 2, PoolIdleTimeout: time.Hour,
	}
	rt, err := a.containerRuntime()
	require.NoError(t, err)
	a.hostPool = newHostPodPool(rt, a.PoolIdleTimeout, a.PoolMaxIdle)
	t.Cleanup(func() { a.hostPool.Close(context.Background()) })

	sidecarScript := `echo boot-out; echo boot-err >&2; i=0; while true; do sleep 1; if [ -f /workspace/on ]; then i=$((i+1)); echo $i > /tmp/count; echo tick-$i >&2; fi; done`
	pt := func() *dsl.PodTemplate {
		return &dsl.PodTemplate{Reuse: true, Spec: map[string]any{"containers": []any{
			map[string]any{"name": "svc", "image": poolBusybox, "command": []any{"sh", "-c", sidecarScript}},
		}}}
	}
	claim := func(runID string) api.ClaimResponse {
		return api.ClaimResponse{RunID: runID, JobName: "pool-reuse-job", PodTemplate: pt(),
			Stages: []api.ClaimStage{{Step: &api.ClaimStep{Index: 0, Name: "wait", Run: "touch /workspace/on; sleep 3"}}}}
	}
	wd := rootCleanedTempDir(t, a)
	sidecarIdx := dsl.SidecarLogIndex(0)

	a.executeRunWith(context.Background(), claim("pool-run-1"), wd, claimOpts{Poolable: true})
	require.Equal(t, "Succeeded", <-h.finish)
	first := h.take(sidecarIdx)
	require.NotEmpty(t, first)
	assert.Contains(t, first, "boot-out", "a fresh pod streams the whole sidecar log")
	maxTick := 0
	for _, l := range first {
		if n, ok := tickNumber(l); ok && n > maxTick {
			maxTick = n
		}
	}
	require.Positive(t, maxTick, "the sidecar ticked during the first run: %v", first)
	require.Equal(t, 1, a.hostPool.Len())
	// runLoop empties the workspace after a run; executeRunWith does not, so do
	// it here: the marker goes away and the idle pod's sidecar falls silent.
	pooled := a.hostPool.idle[wd]
	require.NotNil(t, pooled)
	code, err := rt.Exec(context.Background(), pooled.pod.pause, crt.ExecSpec{Script: "rm -f /workspace/on"}, io.Discard, io.Discard)
	require.NoError(t, err)
	require.Zero(t, code)
	time.Sleep(2500 * time.Millisecond)

	// The last tick the sidecar logged before the second run takes the pod.
	pooled.pod.mu.Lock()
	svc := pooled.pod.open["svc"]
	pooled.pod.mu.Unlock()
	var count strings.Builder
	code, err = rt.Exec(context.Background(), svc, crt.ExecSpec{Script: "cat /tmp/count"}, &count, &count)
	require.NoError(t, err)
	require.Zero(t, code)
	lastBefore, err := strconv.Atoi(strings.TrimSpace(count.String()))
	require.NoError(t, err)
	require.GreaterOrEqual(t, lastBefore, maxTick)

	a.executeRunWith(context.Background(), claim("pool-run-2"), wd, claimOpts{Poolable: true})
	require.Equal(t, "Succeeded", <-h.finish)
	second := h.take(sidecarIdx)
	require.NotEmpty(t, second, "the second run sees its own sidecar output")
	for i, l := range second {
		assert.NotContains(t, l, "boot-", "the previous run's output is not replayed: %q", l)
		n, ok := tickNumber(l)
		require.True(t, ok, "unexpected line %q", l)
		assert.Equal(t, lastBefore+1+i, n, "every line is a new tick, the first right after the last one before the run: %v", second)
	}
	require.Equal(t, 1, a.hostPool.Len(), "the same pod was reused and released again")
}

func tickNumber(line string) (int, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "tick-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil
}

// TestClaimPod_PoolReuse_ExitedSidecarDiscardsPodOnAcquire checks the runtime
// calls the pool relies on: ListContainers by label finds the pod's containers,
// ContainerRunning turns false once the sidecar exits, and Acquire then removes
// the pod instead of handing it out.
func TestClaimPod_PoolReuse_ExitedSidecarDiscardsPodOnAcquire(t *testing.T) {
	rt, err := crt.Detect(testRuntimePref())
	if err != nil {
		skipOrFailInCI(t, "no container runtime available (%s=%q): %v", containerRuntimeEnv, testRuntimePref(), err)
	}
	pr, ok := rt.(crt.PodRuntime)
	require.True(t, ok, "the %s runtime must implement PodRuntime", rt.Name())
	ctx := context.Background()
	_, err = pullClaimImages(ctx, rt, []string{poolBusybox}) // pulls only when absent
	require.NoError(t, err)

	const agentID = "pool-reuse-acquire-agent"
	m := newClaimPodManager(rt, rootCleanedTempDir(t, &Agent{RuntimePref: testRuntimePref()}), "/workspace", poolBusybox, poolBusybox, installShimOrSkip(t))
	m.agentID = agentID
	t.Cleanup(func() { m.CloseAll(context.Background()) })
	pt := &dsl.PodTemplate{Reuse: true, Spec: map[string]any{"containers": []any{
		map[string]any{"name": "svc", "image": poolBusybox, "command": []any{"sh", "-c",
			"while [ ! -f /workspace/stop ]; do sleep 0.2; done"}},
	}}}
	require.NoError(t, m.Start(ctx, pt))

	infos, err := pr.ListContainers(ctx, map[string]string{labelAgentID: agentID, labelClaimPod: "true"})
	require.NoError(t, err)
	assert.Len(t, infos, 3, "pause, sidecar and job container carry the claim labels")

	pool := newHostPodPool(rt, time.Hour, 2)
	assert.True(t, podRunning(ctx, pr, m), "everything is up right after Start")
	pool.Release(ctx, m.workDir, "job", "fp", m, true)

	m.mu.Lock()
	svc := m.open["svc"]
	m.mu.Unlock()
	// Make the sidecar exit now: the pause container shares the workspace.
	code, err := rt.Exec(ctx, m.pause, crt.ExecSpec{Script: "touch /workspace/stop"}, io.Discard, io.Discard)
	require.NoError(t, err)
	require.Zero(t, code)
	require.Eventually(t, func() bool {
		up, err := pr.ContainerRunning(ctx, svc)
		return err == nil && !up
	}, 30*time.Second, 200*time.Millisecond, "the sidecar exits once told to")

	got, hit := pool.Acquire(ctx, m.workDir, "job", "fp")
	assert.False(t, hit, "a pod with an exited sidecar is never handed out")
	assert.Nil(t, got)
	assert.Equal(t, 0, pool.Len())
	infos, err = pr.ListContainers(ctx, map[string]string{labelAgentID: agentID, labelClaimPod: "true"})
	require.NoError(t, err)
	assert.Empty(t, infos, "the discarded pod's containers are removed")
}
