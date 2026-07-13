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
	"github.com/eirueimi/unified-cd/internal/api"
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
	idx := dsl.SidecarLogIndex(ordinal)
	pusher := agentlib.NewLogPusher(p.logs, p.agentID, p.runID, idx, "stdout")
	if p.masker != nil {
		pusher.SetMasker(p.masker)
	}
	pusher.StartAutoFlush(ctx, stderrAutoFlushInterval) // reuse the package's cadence (test-shortenable)
	// Status reports use a context detached from the stream's cancellation —
	// see the host pump's identical comment in internal/agent/sidecar_logs.go.
	flushCtx := context.WithoutCancel(ctx)
	p.reportStatus(flushCtx, name, idx, "running", nil)
	if err := streamPodContainerLogs(ctx, p.client, p.ns, p.pod, name, p.since, pusher); err != nil {
		slog.Warn("k8s sidecar log stream error", "container", name, "error", err)
	}
	pusher.Flush(flushCtx)
	p.reportStatus(flushCtx, name, idx, "exited", p.exitCode(flushCtx, name))
}

// exitCode fetches the pod and reads the named sidecar container's terminated
// exit code, if any. Best-effort: returns nil (unknown) on any lookup
// failure or if the container hasn't terminated — never blocks status
// reporting on this.
func (p *k8sSidecarPump) exitCode(ctx context.Context, name string) *int {
	pod, err := p.client.CoreV1().Pods(p.ns).Get(ctx, p.pod, metav1.GetOptions{})
	if err != nil {
		slog.Warn("k8s sidecar exit-code lookup: get pod failed", "container", name, "error", err)
		return nil
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == name && cs.State.Terminated != nil {
			ec := int(cs.State.Terminated.ExitCode)
			return &ec
		}
	}
	return nil
}

// reportStatus best-effort reports a sidecar's phase/exit-code to the
// controller for UI display. A report failure is logged and dropped: it must
// never fail or slow the run — status is a display concern only.
func (p *k8sSidecarPump) reportStatus(ctx context.Context, name string, idx int, phase string, exitCode *int) {
	req := api.SidecarStatusRequest{RunID: p.runID, Name: name, Index: idx, Phase: phase, ExitCode: exitCode}
	if err := p.logs.ReportSidecarStatus(ctx, p.agentID, req); err != nil {
		slog.Warn("k8s sidecar status report failed", "container", name, "phase", phase, "error", err)
	}
}
