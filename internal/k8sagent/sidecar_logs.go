package k8sagent

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/secrets"
)

// streamPodContainerLogs copies a single pod container's log stream (k8s merges
// stdout+stderr) to w, following until the container ends or ctx is cancelled.
// Best-effort: returns the stream-open error, if any.
func streamPodContainerLogs(ctx context.Context, client kubernetes.Interface, ns, pod, container string, w io.Writer) error {
	req := client.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, Follow: true})
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
	sidecars []string // user sidecar container names, declared order

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
	pusher.StartAutoFlush(ctx, 2*time.Second) // reuse agentlib's cadence
	if err := streamPodContainerLogs(ctx, p.client, p.ns, p.pod, name, pusher); err != nil {
		slog.Warn("k8s sidecar log stream error", "container", name, "error", err)
	}
	pusher.Flush(context.WithoutCancel(ctx))
}
