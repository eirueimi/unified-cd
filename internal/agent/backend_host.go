package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/cache"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/eirueimi/unified-cd/internal/secrets"
)

// hostBackend is the ExecBackend implementation for the host (bare-process)
// agent. It owns the claim-scoped scopeManager (lazily created on first
// uses-scope step) and the secret masker used by StepLogWriters.
type hostBackend struct {
	a       *Agent
	runID   string
	workDir string

	scopesMu sync.Mutex
	scopes   *scopeManager

	masker *secrets.Masker
}

// newHostBackend constructs the ExecBackend for one claim's executeRun call.
func newHostBackend(a *Agent, runID, workDir string) *hostBackend {
	return &hostBackend{a: a, runID: runID, workDir: workDir}
}

// hostScopeHandle is the concrete payload behind ScopeHandle on the host
// backend: the claim's scopeManager plus the specific scope container handle
// resolved for one step.
type hostScopeHandle struct {
	sm *scopeManager
	h  crt.ContainerHandle
}

func wrapHostScope(sm *scopeManager, h crt.ContainerHandle) ScopeHandle {
	if sm == nil {
		return ScopeHandle{}
	}
	return ScopeHandle{opaque: hostScopeHandle{sm: sm, h: h}}
}

func unwrapHostScope(sh ScopeHandle) (*scopeManager, crt.ContainerHandle, bool) {
	if sh.IsZero() {
		return nil, crt.ContainerHandle{}, false
	}
	hs, ok := sh.opaque.(hostScopeHandle)
	if !ok {
		return nil, crt.ContainerHandle{}, false
	}
	return hs.sm, hs.h, true
}

// getScopes returns the claim's scopeManager, creating it lazily on first use.
// See scopeManager's doc comment for the concurrency rationale (parallel:
// stages may call this from multiple goroutines at once).
func (b *hostBackend) getScopes() (*scopeManager, error) {
	b.scopesMu.Lock()
	defer b.scopesMu.Unlock()
	if b.scopes != nil {
		return b.scopes, nil
	}
	rt, err := b.a.containerRuntime()
	if err != nil {
		return nil, fmt.Errorf("uses-scope requires a container runtime: %w", err)
	}
	b.scopes = newScopeManager(rt)
	return b.scopes, nil
}

// RunDefault runs a step directly on the host workspace.
func (b *hostBackend) RunDefault(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return RunStep(ctx, script, stdout, stderr, env, b.workDir)
}

// RunImage runs a step in a fresh, unmounted container (runsIn.image).
func (b *hostBackend) RunImage(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error) {
	rt, err := b.a.containerRuntime()
	if err != nil {
		return -1, fmt.Errorf("runsIn.image %q requires a container runtime: %w", step.RunsIn.Image, err)
	}
	cpuLimit, memLimit := hostContainerLimits(step.RunsIn.Resources)
	return RunStepContainer(ctx, rt, step.RunsIn.Image, script, stdout, stderr, env, cpuLimit, memLimit)
}

// RunNamedContainer is not supported on the host agent: runsIn.container
// targets a long-lived named container that only the k8s agent's sidecar
// model can provide.
func (b *hostBackend) RunNamedContainer(ctx context.Context, step api.ClaimStep, container, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return -1, fmt.Errorf("runsIn.container (%q) is not supported on the host agent; use runsIn.image or the k8s agent", container)
}

// EnsureScope provisions (or reuses) the step's uses-scope container.
func (b *hostBackend) EnsureScope(ctx context.Context, step api.ClaimStep, env []string) (ScopeHandle, error) {
	sm, err := b.getScopes()
	if err != nil {
		return ScopeHandle{}, err
	}
	h, err := sm.ensure(ctx, step, env)
	if err != nil {
		return ScopeHandle{}, err
	}
	return wrapHostScope(sm, h), nil
}

// RunInScope executes script inside the scope container identified by h.
func (b *hostBackend) RunInScope(ctx context.Context, h ScopeHandle, script string, env []string, stdout, stderr io.Writer) (int, error) {
	sm, handle, ok := unwrapHostScope(h)
	if !ok {
		return -1, fmt.Errorf("RunInScope: no scope handle")
	}
	return sm.exec(ctx, handle, script, env, stdout, stderr)
}

// CloseScopes tears down every scope container opened during the claim.
func (b *hostBackend) CloseScopes(ctx context.Context) {
	b.scopesMu.Lock()
	scopes := b.scopes
	b.scopesMu.Unlock()
	if scopes != nil {
		scopes.closeAll(ctx)
	}
}

// CacheRestore restores a cache entry into path (host workspace path, or
// scope-container path when scope is non-zero), reporting a cache hit.
func (b *hostBackend) CacheRestore(ctx context.Context, scope ScopeHandle, key string, restoreKeys []string, path string) (bool, error) {
	if b.a.CacheStore == nil {
		return false, nil
	}
	sm, h, ok := unwrapHostScope(scope)
	if !ok {
		return cache.Restore(ctx, b.a.CacheStore, path, key, restoreKeys)
	}
	hostDir, err := os.MkdirTemp("", "ucd-scope-cache-restore-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(hostDir)
	hit, err := cache.Restore(ctx, b.a.CacheStore, hostDir, key, restoreKeys)
	if err != nil && !errors.Is(err, cache.ErrCacheMiss) {
		return false, err
	}
	if !hit {
		return false, nil
	}
	if err := sm.copyIn(ctx, h, hostDir, path); err != nil {
		return false, err
	}
	return true, nil
}

// CacheSave archives path (host workspace path, or scope-container path when
// scope is non-zero) under key.
func (b *hostBackend) CacheSave(ctx context.Context, scope ScopeHandle, key, path string, ttlDays int) error {
	if b.a.CacheStore == nil {
		return nil
	}
	sm, h, ok := unwrapHostScope(scope)
	if !ok {
		return cache.Save(ctx, b.a.CacheStore, path, key, ttlDays)
	}
	hostPath, cleanup, err := sm.copyOutToTemp(ctx, h, path)
	if err != nil {
		return err
	}
	defer cleanup()
	return cache.Save(ctx, b.a.CacheStore, hostPath, key, ttlDays)
}

// UploadArtifact uploads the file/dir at path (host or scope-container path)
// to the master server as an artifact named name.
func (b *hostBackend) UploadArtifact(ctx context.Context, scope ScopeHandle, runID, name, path string) error {
	sm, h, ok := unwrapHostScope(scope)
	if !ok {
		return b.a.Client.UploadArtifact(ctx, runID, name, path)
	}
	artifactPath, cleanup, err := sm.copyOutToTemp(ctx, h, path)
	if err != nil {
		return fmt.Errorf("copy from scope: %w", err)
	}
	defer cleanup()
	return b.a.Client.UploadArtifact(ctx, runID, name, artifactPath)
}

// DownloadArtifact downloads the artifact named name into destDir (host or
// scope-container path).
func (b *hostBackend) DownloadArtifact(ctx context.Context, scope ScopeHandle, runID, name, destDir string) error {
	sm, h, ok := unwrapHostScope(scope)
	if !ok {
		return b.a.Client.DownloadArtifact(ctx, runID, name, destDir)
	}
	tmp, err := os.MkdirTemp("", "ucd-scope-download-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	if err := b.a.Client.DownloadArtifact(ctx, runID, name, tmp); err != nil {
		return err
	}
	if err := sm.copyIn(ctx, h, tmp, destDir); err != nil {
		return fmt.Errorf("copy into scope: %w", err)
	}
	return nil
}

// RunPostHook runs a step's post: script after the step succeeds. Scoped
// steps run their post hook inside the same scope container the step body
// ran in; container is unused on the host backend (named containers are not
// supported here — see RunNamedContainer).
func (b *hostBackend) RunPostHook(ctx context.Context, scope ScopeHandle, container, script string, env []string) error {
	sm, h, ok := unwrapHostScope(scope)
	if ok {
		_, err := sm.exec(ctx, h, script, env, nil, nil)
		return err
	}
	_, _, err := RunStepCapture(ctx, script, nil, env, b.workDir)
	return err
}

// SetMasker installs the secret masker used by subsequently-created log
// writers (see StepLogWriters).
func (b *hostBackend) SetMasker(m *secrets.Masker) {
	b.masker = m
}

// StepLogWriters returns LogPushers for stdout/stderr, auto-flushing every
// logPusherAutoFlushEvery until finish is called. finish stops the auto-flush
// goroutine and does a final Flush of both streams.
func (b *hostBackend) StepLogWriters(ctx context.Context, stepIndex int) (stdout, stderr io.Writer, finish func(ctx context.Context)) {
	stderrPusher := NewLogPusher(b.a.Client, b.a.ID, b.runID, stepIndex, "stderr")
	stderrPusher.SetMasker(b.masker)
	stdoutPusher := NewLogPusher(b.a.Client, b.a.ID, b.runID, stepIndex, "stdout")
	stdoutPusher.SetMasker(b.masker)
	flushCtx, stopAutoFlush := context.WithCancel(ctx)
	stderrPusher.StartAutoFlush(flushCtx, logPusherAutoFlushEvery)
	stdoutPusher.StartAutoFlush(flushCtx, logPusherAutoFlushEvery)

	finish = func(finishCtx context.Context) {
		stopAutoFlush()
		stderrPusher.Flush(finishCtx)
		stdoutPusher.Flush(finishCtx)
	}
	return stdoutPusher, stderrPusher, finish
}

// ConcurrencyMode reports how the host agent runs parallel-group / matrix
// members: concurrently, as goroutines (its historical behavior).
func (b *hostBackend) ConcurrencyMode() ConcurrencyMode {
	return Concurrent
}

var _ ExecBackend = (*hostBackend)(nil)
