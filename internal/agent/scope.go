package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/eirueimi/unified-cd/internal/api"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

// scopeManager owns the isolated environments for uses-level runsIn.image
// scopes on the host agent. One environment per (ScopeID, MatrixKey).
type scopeManager struct {
	rt   crt.ContainerRuntime
	open map[string]crt.ContainerHandle
}

func newScopeManager(rt crt.ContainerRuntime) *scopeManager {
	return &scopeManager{rt: rt, open: map[string]crt.ContainerHandle{}}
}

func (m *scopeManager) key(step api.ClaimStep) string {
	return step.ScopeID + "\x00" + step.MatrixKey
}

// ensure returns the scope container for step, creating it on first use.
func (m *scopeManager) ensure(ctx context.Context, step api.ClaimStep, env []string) (crt.ContainerHandle, error) {
	k := m.key(step)
	if h, ok := m.open[k]; ok {
		return h, nil
	}
	h, err := m.rt.Create(ctx, crt.CreateSpec{Image: step.ScopeImage, Env: env})
	if err != nil {
		return crt.ContainerHandle{}, fmt.Errorf("provision scope %q (image %q): %w", step.ScopeID, step.ScopeImage, err)
	}
	m.open[k] = h
	return h, nil
}

func (m *scopeManager) exec(ctx context.Context, h crt.ContainerHandle, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return m.rt.Exec(ctx, h, crt.ExecSpec{Script: script, Env: env}, stdout, stderr)
}

// copyOutToTemp copies a container path to a fresh host temp dir and returns
// the host path plus a cleanup func.
func (m *scopeManager) copyOutToTemp(ctx context.Context, h crt.ContainerHandle, containerPath string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "ucd-scope-out-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	dst := dir + string(os.PathSeparator) + "artifact"
	if err := m.rt.CopyOut(ctx, h, containerPath, dst); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return dst, cleanup, nil
}

func (m *scopeManager) copyIn(ctx context.Context, h crt.ContainerHandle, hostPath, containerPath string) error {
	return m.rt.CopyIn(ctx, h, hostPath, containerPath)
}

func (m *scopeManager) closeKey(ctx context.Context, key string) {
	h, ok := m.open[key]
	if !ok {
		return
	}
	if err := m.rt.Remove(ctx, h); err != nil {
		slog.Warn("scope teardown failed", "container", h.ID, "error", err)
	}
	delete(m.open, key)
}

func (m *scopeManager) closeAll(ctx context.Context) {
	for k := range m.open {
		m.closeKey(ctx, k)
	}
}
