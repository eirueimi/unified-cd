package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/eirueimi/unified-cd/internal/api"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

// scopeManager owns the isolated environments for uses-level runsIn.image
// scopes on the host agent. One environment per (ScopeID, MatrixKey).
//
// A single claim's steps may run concurrently (parallel: stages execute as
// goroutines, see pipeline.go's runParallel), and multiple of those steps can
// share a scope key or race to provision distinct keys at the same time. mu
// guards every access to open so that concurrent ensure/closeKey/closeAll
// calls are race-free and each key is provisioned at most once.
//
// mu is held across the check-and-create in ensure (including the
// rt.Create call), which is the simplest correct option: it serializes scope
// provisioning within a claim, trading a small amount of parallelism (two
// distinct scopes cannot be created at the exact same instant) for a
// guarantee that a key is never double-created and open is never touched
// without the lock. Scope provisioning happens once per key per claim, so
// this serialization is not expected to be a meaningful bottleneck.
type scopeManager struct {
	rt crt.ContainerRuntime
	// toolsDir is the host tools dir the agent wrote the embedded ucd-sh
	// shim into (see Agent.InstallShim); every scope container bind-mounts
	// it read-only at /.ucd, same as claim-pod containers (claim_pod.go).
	// Empty means "no shim mount" (see ucdToolsMount).
	toolsDir string
	mu       sync.Mutex
	open     map[string]crt.ContainerHandle
}

func newScopeManager(rt crt.ContainerRuntime, toolsDir string) *scopeManager {
	return &scopeManager{rt: rt, toolsDir: toolsDir, open: map[string]crt.ContainerHandle{}}
}

// isScopedStep reports whether step targets an isolated uses-scope container
// rather than the shared host workspace. This is the same routing signal the
// k8s agent uses (step.ScopeID != "") for backend parity.
func isScopedStep(step api.ClaimStep) bool { return step.ScopeID != "" }

// scopeWorkDir is the fixed working directory for a host uses-scope
// container, mirroring the k8s agent's scopeMountPath (internal/k8sagent/
// scopepod.go). Without a defined working directory, scoped `run:` steps and
// `docker exec` have no cwd, and relative container-side paths handed to
// `docker cp` are rejected ("destination path must be absolute"). Container
// paths for scoped artifact/cache operations are resolved against this
// constant (via path.Join, not filepath.Join — the container is always
// Linux, forward-slash, regardless of host OS) before copyIn/copyOut.
const scopeWorkDir = "/workspace"

func (m *scopeManager) key(step api.ClaimStep) string {
	return step.ScopeID + "\x00" + step.MatrixKey
}

// ensure returns the scope container for step, creating it on first use.
// The lock is held across the check-and-create (including the rt.Create
// call) so that concurrent callers racing on the same key never both
// observe a miss and double-create a container; see the scopeManager doc
// comment for the tradeoff this implies.
func (m *scopeManager) ensure(ctx context.Context, step api.ClaimStep, env []string) (crt.ContainerHandle, error) {
	k := m.key(step)
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.open[k]; ok {
		return h, nil
	}
	// Command is the explicit ucd-sh pause keep-alive: this container is an
	// exec target for scoped steps (see crt.CreateSpec.Command), not a
	// service sidecar, so it must not run the image's default entrypoint.
	// The /.ucd mount makes the keep-alive binary (and the default step
	// shell) available without any image requirement.
	h, err := m.rt.Create(ctx, crt.CreateSpec{Image: step.ScopeImage, Env: env, WorkDir: scopeWorkDir, Command: ucdShPause, Mounts: ucdToolsMount(m.toolsDir)})
	if err != nil {
		return crt.ContainerHandle{}, fmt.Errorf("provision scope %q (image %q): %w", step.ScopeID, step.ScopeImage, err)
	}
	m.open[k] = h
	return h, nil
}

// exec runs script inside h. shell is the step's effective interpreter argv
// (nil/empty resolves to the shim default — see effectiveShell), always set
// explicitly so the runtime layer stays dumb.
func (m *scopeManager) exec(ctx context.Context, h crt.ContainerHandle, script string, shell, env []string, stdout, stderr io.Writer) (int, error) {
	return m.rt.Exec(ctx, h, crt.ExecSpec{Script: script, Shell: effectiveShell(shell), Env: env}, stdout, stderr)
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
	m.mu.Lock()
	h, ok := m.open[key]
	if ok {
		delete(m.open, key)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	if err := m.rt.Remove(ctx, h); err != nil {
		slog.Warn("scope teardown failed", "container", h.ID, "error", err)
	}
}

func (m *scopeManager) closeAll(ctx context.Context) {
	m.mu.Lock()
	keys := make([]string, 0, len(m.open))
	for k := range m.open {
		keys = append(keys, k)
	}
	m.mu.Unlock()
	for _, k := range keys {
		m.closeKey(ctx, k)
	}
}
