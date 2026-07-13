package agent

import (
	"context"
	"log/slog"
	"sync"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/eirueimi/unified-cd/internal/secrets"
)

// SidecarHandle names one user podTemplate sidecar container to stream: its
// declared name, its 0-based ordinal among non-"job" containers (→ log index
// via dsl.SidecarLogIndex), and its runtime handle.
type SidecarHandle struct {
	Name    string
	Ordinal int
	Handle  crt.ContainerHandle
}

// sidecarLogPump streams each user sidecar container's stdout/stderr into the
// run log store under the sidecar's sentinel step index, for the run's lifetime.
// Best-effort: a stream that errors is logged and dropped; the run never fails.
type sidecarLogPump struct {
	rt       crt.ContainerRuntime
	client   *Client
	agentID  string
	runID    string
	masker   *secrets.Masker
	sidecars []SidecarHandle

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newSidecarLogPump(rt crt.ContainerRuntime, client *Client, agentID, runID string, masker *secrets.Masker, sidecars []SidecarHandle) *sidecarLogPump {
	return &sidecarLogPump{rt: rt, client: client, agentID: agentID, runID: runID, masker: masker, sidecars: sidecars}
}

// Start spawns one streaming goroutine per sidecar. Idempotent-safe to call once.
func (p *sidecarLogPump) Start(ctx context.Context) {
	streamCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	for _, sc := range p.sidecars {
		p.wg.Add(1)
		go p.stream(streamCtx, sc)
	}
}

// Stop cancels all streams and waits for their goroutines (flushing final logs).
func (p *sidecarLogPump) Stop(ctx context.Context) {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

func (p *sidecarLogPump) stream(ctx context.Context, sc SidecarHandle) {
	defer p.wg.Done()
	idx := dsl.SidecarLogIndex(sc.Ordinal)
	stdout := NewLogPusher(p.client, p.agentID, p.runID, idx, "stdout")
	stderr := NewLogPusher(p.client, p.agentID, p.runID, idx, "stderr")
	if p.masker != nil {
		stdout.SetMasker(p.masker)
		stderr.SetMasker(p.masker)
	}
	stdout.StartAutoFlush(ctx, logPusherAutoFlushEvery)
	stderr.StartAutoFlush(ctx, logPusherAutoFlushEvery)
	// Status reports use a context detached from the stream's cancellation:
	// Stop() cancels ctx to unblock rt.Logs, but that same cancellation must
	// not race the "running" report (sent just before rt.Logs starts) or
	// abort the final "exited" report — both are best-effort notifications
	// to the controller, not part of the stream itself.
	flushCtx := context.WithoutCancel(ctx)
	p.reportStatus(flushCtx, sc, idx, "running", nil)
	if err := p.rt.Logs(ctx, sc.Handle, stdout, stderr); err != nil {
		slog.Warn("sidecar log stream ended with error", "container", sc.Name, "error", err)
	}
	// Flush remainder with a live (non-cancelled) context so final lines ship.
	stdout.Flush(flushCtx)
	stderr.Flush(flushCtx)
	var exitCode *int
	if ec, err := p.rt.ExitCode(flushCtx, sc.Handle); err != nil {
		slog.Warn("sidecar exit-code lookup failed", "container", sc.Name, "error", err)
	} else {
		exitCode = &ec
	}
	p.reportStatus(flushCtx, sc, idx, "exited", exitCode)
}

// reportStatus best-effort reports a sidecar's phase/exit-code to the
// controller for UI display. A report failure is logged and dropped: it must
// never fail or slow the run — status is a display concern only.
func (p *sidecarLogPump) reportStatus(ctx context.Context, sc SidecarHandle, idx int, phase string, exitCode *int) {
	req := api.SidecarStatusRequest{RunID: p.runID, Name: sc.Name, Index: idx, Phase: phase, ExitCode: exitCode}
	if err := p.client.ReportSidecarStatus(ctx, p.agentID, req); err != nil {
		slog.Warn("sidecar status report failed", "container", sc.Name, "phase", phase, "error", err)
	}
}
