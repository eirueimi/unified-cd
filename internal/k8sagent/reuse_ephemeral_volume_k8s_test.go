//go:build k8s

package k8sagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestK8sAgent_PooledReuse_KeepsGenericEphemeralVolume proves that a pooled
// (podTemplate.reuse) pod keeps a generic ephemeral volume across runs: the
// pod, and therefore the PVC the volumeClaimTemplate created for it, survives
// between runs, and only the workspace is emptied. A CSI-clone dataSource is
// not exercised (kind has no CSI clone); the pooling path is what matters.
func TestK8sAgent_PooledReuse_KeepsGenericEphemeralVolume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	client, restCfg := newTestKubeClient(t)
	shimImage := testShimImageOrSkip(t)
	ns := newTestNamespace(t, client)

	const agentID = "k8s-reuse-ephemeral"

	var mu sync.Mutex
	logs := map[string][]string{} // runID -> log lines
	finished := map[string]api.RunStatus{}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{run}/steps/{step}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		if err := json.NewDecoder(r.Body).Decode(&reqs); err == nil {
			mu.Lock()
			for _, req := range reqs {
				if req.Line != "" {
					logs[r.PathValue("run")] = append(logs[r.PathValue("run")], req.Line)
				}
			}
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{run}/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		finished[r.PathValue("run")] = api.RunStatus(body["status"])
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pm := NewPodManager(client, ns, testImage)
	exec := NewExecutor(client, restCfg, ns)
	pool := NewPodPool(client, ns, pm)
	pool.SetIdleTimeout(10 * time.Minute) // pooled, but never evicted during the test
	agentClient := agentlib.NewClient(srv.URL, "tok")

	cfg := Config{
		AgentID:   agentID,
		Namespace: ns,
		PodImage:  testImage,
		ShimImage: shimImage,
		// ephemeral is opt-in: without this the job is refused.
		JobPodPolicy: JobPodPolicy{AllowVolumeTypes: []string{"ephemeral"}},
	}
	a := NewK8sAgent(cfg, agentClient, pm, exec, pool)

	tpl := &dsl.PodTemplate{
		Reuse: true,
		Spec: map[string]any{
			"containers": []any{map[string]any{
				"name":         "job",
				"image":        testImage,
				"volumeMounts": []any{map[string]any{"name": "scratch", "mountPath": "/scratch"}},
			}},
			"volumes": []any{map[string]any{
				"name": "scratch",
				"ephemeral": map[string]any{"volumeClaimTemplate": map[string]any{"spec": map[string]any{
					"accessModes": []any{"ReadWriteOnce"},
					"resources":   map[string]any{"requests": map[string]any{"storage": "64Mi"}},
					// A CSI clone would add: dataSource: {kind: PersistentVolumeClaim, name: <golden>}
				}}},
			}},
		},
	}

	runOnce := func(runID, script string) []string {
		t.Helper()
		a.executeRun(ctx, api.ClaimResponse{
			RunID:       runID,
			JobName:     "reuse-ephemeral",
			PodTemplate: tpl,
			Stages: []api.ClaimStage{
				{Step: &api.ClaimStep{Index: 0, Name: "s", Run: script}},
			},
		})
		// Log pushers flush asynchronously; wait for the finish call.
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			_, ok := finished[runID]
			return ok
		}, 30*time.Second, 200*time.Millisecond, "run %s did not finish", runID)
		mu.Lock()
		defer mu.Unlock()
		require.Equal(t, api.RunSucceeded, finished[runID], "run %s logs: %v", runID, logs[runID])
		return append([]string(nil), logs[runID]...)
	}
	field := func(lines []string, prefix string) string {
		for _, l := range lines {
			if i := strings.Index(l, prefix); i >= 0 {
				return strings.TrimSpace(l[i+len(prefix):])
			}
		}
		return ""
	}

	// Run 1: write a marker into the ephemeral volume and a file into the workspace.
	l1 := runOnce("run-eph-1", `echo "POD=$(hostname)"; echo kept > /scratch/marker; echo ws > ./ws-file; echo "WS=$(ls -A . | wc -l)"`)
	pod1 := field(l1, "POD=")
	require.NotEmpty(t, pod1, "run 1 logs: %v", l1)

	// The generic ephemeral PVC exists, owned by the pod.
	pvc, err := client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pod1+"-scratch", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, pvc.OwnerReferences)
	assert.Equal(t, "Pod", pvc.OwnerReferences[0].Kind)
	assert.Equal(t, pod1, pvc.OwnerReferences[0].Name)
	pvcUID := pvc.UID

	// Run 2: same job, so the same pooled pod. The marker must still be there
	// and the workspace must have been emptied.
	l2 := runOnce("run-eph-2", `echo "POD=$(hostname)"; if [ -f /scratch/marker ]; then echo "MARKER=$(cat /scratch/marker)"; else echo MARKER=absent; fi; echo "WS=$(ls -A . | wc -l)"`)
	assert.Equal(t, pod1, field(l2, "POD="), "run 2 must be served by the same pooled pod; logs: %v", l2)
	assert.Equal(t, "kept", field(l2, "MARKER="), "the ephemeral volume must survive reuse; logs: %v", l2)
	assert.Equal(t, "0", field(l2, "WS="), "the workspace must be emptied between runs; logs: %v", l2)

	// Same PVC object (no new clone was provisioned).
	pvc2, err := client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pod1+"-scratch", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, pvcUID, pvc2.UID)
}
