package k8sagent

import (
	"context"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// podGCDecision reports whether a run Pod should be deleted by the orphan-pod
// GC: never for a Pod that belongs to the reuse pool (pooledInUse — the pool's
// own idle-timeout/Restore logic owns its lifecycle), and otherwise iff the
// backing Run is gone (found=false) or has reached a terminal status.
func podGCDecision(runStatus api.RunStatus, found bool, pooledInUse bool) bool {
	if pooledInUse {
		return false
	}
	return !found || isTerminalRunStatus(runStatus)
}

// isTerminalRunStatus reports whether a Run has finished executing.
func isTerminalRunStatus(status api.RunStatus) bool {
	switch status {
	case api.RunSucceeded, api.RunFailed, api.RunCancelled:
		return true
	default:
		return false
	}
}

// gcPod is a run Pod as seen by the GC's pod lister: its name, the runId it
// was created for, and whether it is currently managed by the reuse pool
// (carries the pool-template annotation, idle or in-use) and so must be left
// alone.
type gcPod struct {
	podName     string
	runID       string
	pooledInUse bool
}

// podLister lists candidate run Pods for the orphan-pod GC.
type podLister func(ctx context.Context) ([]gcPod, error)

// runGetter fetches a Run's current status. A not-found (or any other) error
// is treated as found=false by the caller — an unreachable/gone Run is itself
// grounds for reclaiming its Pod.
type runGetter func(ctx context.Context, runID string) (api.Run, error)

// podDeleter deletes a Pod by name.
type podDeleter func(ctx context.Context, podName string) error

// runPodGCOnce lists run Pods via lister, resolves each one's backing Run via
// getRun, and deletes (via deletePod) any Pod whose Run is terminal or absent,
// skipping Pods the reuse pool still manages. Best-effort per Pod: a
// deletion failure is logged and does not stop the sweep.
func runPodGCOnce(ctx context.Context, lister podLister, getRun runGetter, deletePod podDeleter) error {
	pods, err := lister(ctx)
	if err != nil {
		return err
	}
	for _, p := range pods {
		run, err := getRun(ctx, p.runID)
		found := err == nil
		if !podGCDecision(run.Status, found, p.pooledInUse) {
			continue
		}
		if err := deletePod(ctx, p.podName); err != nil {
			slog.Warn("k8s: pod GC delete failed", "pod", p.podName, "runId", p.runID, "error", err)
			continue
		}
		slog.Info("k8s: pod GC deleted orphaned pod", "pod", p.podName, "runId", p.runID, "runFound", found)
	}
	return nil
}

// runPodGC periodically sweeps run Pods (label app=unified-cd-agent) and
// deletes ones whose backing Run has finished or no longer exists, so a Pod
// is never stranded when a Run's normal completion path (executeRun's
// deferred delete/release) didn't run — e.g. the agent process restarted or
// crashed mid-run. Pods managed by the reuse pool are left to the pool's own
// idle-timeout/Restore logic.
func (a *K8sAgent) runPodGC(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := runPodGCOnce(ctx, a.listRunPods, a.client.GetRun, a.pm.DeletePod); err != nil {
			slog.Error("k8s: pod GC list failed", "error", err)
		}
	}
}

// listRunPods lists this namespace's run Pods (app=unified-cd-agent) and
// extracts each one's runId label and pool-in-use status for the GC sweep.
func (a *K8sAgent) listRunPods(ctx context.Context) ([]gcPod, error) {
	pods, err := a.pm.client.CoreV1().Pods(a.pm.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=unified-cd-agent",
	})
	if err != nil {
		return nil, err
	}
	out := make([]gcPod, 0, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		runID := pod.Labels["unified-cd/runId"]
		if runID == "" {
			continue
		}
		pooled := pod.Annotations[annoPoolTemplate] != ""
		out = append(out, gcPod{podName: pod.Name, runID: runID, pooledInUse: pooled})
	}
	return out, nil
}
