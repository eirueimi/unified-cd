package k8sagent

import (
	"context"
	"io"
	"log/slog"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/secrets"
)

// streamPodContainerLogs copies a single pod container's log stream (k8s merges
// stdout+stderr) to w, following until the container ends or ctx is cancelled.
// SinceTime bounds the replay to logs at or after `since` — essential for
// pooled pods, whose sidecar containers are reused (never restarted) across
// runs, so without it every claim would replay a previous run's output.
// Best-effort: returns the stream-open error, if any.
func streamPodContainerLogs(ctx context.Context, client kubernetes.Interface, ns, pod, container string, since metav1.Time, w io.Writer) error {
	req := client.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, Follow: true, SinceTime: &since})
	rc, err := req.Stream(ctx)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, _ = io.Copy(w, rc)
	return nil
}

// k8sSidecarPump streams every user sidecar container's logs into the run store
// under the sidecar's sentinel index. Mirrors the host sidecarLogPump.
type k8sSidecarPump struct {
	client   kubernetes.Interface
	logs     *agentlib.Client
	ns       string
	pod      string
	agentID  string
	runID    string
	masker   *secrets.Masker
	sidecars []string    // user sidecar container names, declared order
	since    metav1.Time // stream only logs at/after this run's claim time

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (p *k8sSidecarPump) Start(ctx context.Context) {
	streamCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	for i, name := range p.sidecars {
		p.wg.Add(1)
		go p.stream(streamCtx, i, name)
	}
}

func (p *k8sSidecarPump) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

func (p *k8sSidecarPump) stream(ctx context.Context, ordinal int, name string) {
	defer p.wg.Done()
	pusher := agentlib.NewLogPusher(p.logs, p.agentID, p.runID, dsl.SidecarLogIndex(ordinal), "stdout")
	if p.masker != nil {
		pusher.SetMasker(p.masker)
	}
	pusher.StartAutoFlush(ctx, stderrAutoFlushInterval) // reuse the package's cadence (test-shortenable)
	if err := streamPodContainerLogs(ctx, p.client, p.ns, p.pod, name, p.since, pusher); err != nil {
		slog.Warn("k8s sidecar log stream error", "container", name, "error", err)
	}
	pusher.Flush(context.WithoutCancel(ctx))
}
