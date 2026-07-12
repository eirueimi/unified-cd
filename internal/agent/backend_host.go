package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/eirueimi/unified-cd/internal/secrets"
)

// runStepFn / runStepWithShellFn are indirected for testability, mirroring
// this codebase's existing exec-seam pattern (e.g. runtime.execCommand,
// runtime.lookPath): hostBackend's native-path dispatch tests (nil step.Shell
// -> RunStep, non-nil -> RunStepWithShell with the declared argv) swap these
// to observe which path was taken and with what arguments, without spawning
// a real host process.
var runStepFn = RunStep
var runStepWithShellFn = RunStepWithShell

// hostBackend is the ExecBackend implementation for the host (bare-process)
// agent. It owns the claim-scoped scopeManager (uses-scope containers), created
// lazily on first use, plus the secret masker used by StepLogWriters.
//
// pod is the claim pod backing an ISOLATED claim (nil for native: true). When
// set, every default step execs into its primary "job" container, container:
// steps exec into the named container, DefaultAgentOS reports "linux", and
// non-scoped cache paths resolve against the claim workspace (the bind mount
// makes the host workDir and the in-container mountPath the same tree). When
// nil, the backend keeps today's native behavior exactly: default steps run as
// host processes, container: steps error, DefaultAgentOS is runtime.GOOS, and
// non-scoped cache paths are left as authored.
type hostBackend struct {
	a       *Agent
	runID   string
	workDir string
	pod     *claimPodManager

	scopesMu sync.Mutex
	scopes   *scopeManager

	masker *secrets.Masker
}

// newHostBackend constructs the ExecBackend for one claim's executeRun call.
// pod is the claim pod for an isolated claim, or nil for a native (native:
// true) claim.
func newHostBackend(a *Agent, runID, workDir string, pod *claimPodManager) *hostBackend {
	return &hostBackend{a: a, runID: runID, workDir: workDir, pod: pod}
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
	b.scopes = newScopeManager(rt, b.a.ToolsDir)
	return b.scopes, nil
}

// RunDefault runs a default step: for an isolated claim it execs into the
// pod's primary ("job") container (with step.Shell threaded through, nil
// meaning the shim default); for a native claim it runs directly on the host
// workspace — today's bash path (RunStep) when step.Shell is unset, or the
// declared interpreter argv (RunStepWithShell) when set.
func (b *hostBackend) RunDefault(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error) {
	if b.pod != nil {
		return b.pod.Exec(ctx, "", script, step.Shell, env, stdout, stderr)
	}
	if len(step.Shell) > 0 {
		return runStepWithShellFn(ctx, step.Shell, script, stdout, stderr, env, b.workDir)
	}
	return runStepFn(ctx, script, stdout, stderr, env, b.workDir)
}

// hostNamedMountPath is the in-container path the host workspace is bind-mounted
// at for the claim pod's containers. It mirrors the k8s workspace mount
// (podbuilder.injectWorkspace): /workspace unless the podTemplate overrides it.
func hostNamedMountPath(pt *dsl.PodTemplate) string {
	if pt != nil && pt.Workspace != nil && pt.Workspace.MountPath != "" {
		return pt.Workspace.MountPath
	}
	return "/workspace"
}

// RunNamedContainer runs a container: step in the claim pod's named container,
// defined in the claim's podTemplate and sharing the workspace via the pod
// bind mount. This is the host counterpart to the k8s agent's
// exec-into-named-pod-container behavior. A native claim has no pod, so a
// container: step is an error there (no silent host fallback).
func (b *hostBackend) RunNamedContainer(ctx context.Context, step api.ClaimStep, container, script string, env []string, stdout, stderr io.Writer) (int, error) {
	if b.pod == nil {
		return -1, fmt.Errorf("container: %q requires an isolated job (this claim is native)", container)
	}
	return b.pod.Exec(ctx, container, script, step.Shell, env, stdout, stderr)
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
func (b *hostBackend) RunInScope(ctx context.Context, h ScopeHandle, script string, shell []string, env []string, stdout, stderr io.Writer) (int, error) {
	sm, handle, ok := unwrapHostScope(h)
	if !ok {
		return -1, fmt.Errorf("RunInScope: no scope handle")
	}
	return sm.exec(ctx, handle, script, shell, env, stdout, stderr)
}

// CloseScopes tears down every scope container opened during the claim and, for
// an isolated claim, the claim pod (all containers plus the pause netns owner).
func (b *hostBackend) CloseScopes(ctx context.Context) {
	b.scopesMu.Lock()
	scopes := b.scopes
	b.scopesMu.Unlock()
	if scopes != nil {
		scopes.closeAll(ctx)
	}

	if b.pod != nil {
		b.pod.CloseAll(ctx)
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
		hit, err := cache.Restore(ctx, b.a.CacheStore, path, key, restoreKeys)
		if err != nil && !errors.Is(err, cache.ErrCacheMiss) {
			return false, err
		}
		return hit, nil
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
// scope is non-zero) under key. A nil CacheStore (cache disabled) is a
// silent no-op from this backend's point of view — see the doc comment on
// the orchestrator's deferred-save log line (orchestrator.go) for why the
// caller still logs "cache saved" in this case (imprecise but harmless: no
// interface change was justified for a Minor-severity log message).
func (b *hostBackend) CacheSave(ctx context.Context, scope ScopeHandle, key, path string, ttlDays int) error {
	if b.a.CacheStore == nil {
		slog.Debug("cache disabled; save skipped", "key", key)
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

// ResolveArtifactPath resolves p against the host workDir (non-scoped) or the
// scope container's fixed working directory (scoped), mirroring the
// pre-refactor resolveWorkspacePath/resolveScopePath pair.
func (b *hostBackend) ResolveArtifactPath(scope ScopeHandle, p string) string {
	if !scope.IsZero() {
		return resolveScopePath(p)
	}
	return resolveWorkspacePath(b.workDir, p)
}

// ResolveCachePath resolves p against the scope container's fixed working
// directory when scoped (identical to ResolveArtifactPath). For a non-scoped p
// the behavior branches on the claim kind:
//   - isolated: resolve against the claim workspace like the k8s backend joins
//     the pod mount path; the bind mount makes the host workDir and the
//     in-container mountPath the same tree, so a relative cache path must be
//     anchored to it (matching every other pod-semantics path).
//   - native: leave p UNRESOLVED (as authored), matching the pre-refactor host
//     agent's cache.Restore/cache.Save calls, which treat it as relative to the
//     objectstore's own root rather than the claim's workDir.
func (b *hostBackend) ResolveCachePath(scope ScopeHandle, p string) string {
	if !scope.IsZero() {
		return resolveScopePath(p)
	}
	if b.pod != nil {
		return resolveWorkspacePath(b.workDir, p)
	}
	return p
}

// DefaultAgentOS reports "linux" for an isolated claim (its default steps exec
// inside the Linux claim pod, mirroring the k8s agent) and the host process's
// own OS for a native claim (its default steps run directly on the host).
func (b *hostBackend) DefaultAgentOS() string {
	if b.pod != nil {
		return "linux"
	}
	return runtime.GOOS
}

// RunPostHook runs a step's post: script after the step succeeds. A scoped step
// runs its post inside the same scope container. For an isolated claim every
// other post runs in the claim pod — into the step's named container
// (container != "") or the primary "job" container (container == ""), matching
// k8s where every default exec targets the primary; the container is still
// alive (the pod is torn down only at claim end). For a native claim a
// non-scoped post runs on the host workspace. stdout/stderr are the owning
// step's shipping writers (see ExecBackend.RunPostHook's doc comment) — every
// path below feeds the script's real output into them instead of discarding
// it.
func (b *hostBackend) RunPostHook(ctx context.Context, scope ScopeHandle, container, script string, shell []string, env []string, stdout, stderr io.Writer) error {
	if sm, h, ok := unwrapHostScope(scope); ok {
		_, err := sm.exec(ctx, h, script, shell, env, stdout, stderr)
		return err
	}
	if b.pod != nil {
		_, err := b.pod.Exec(ctx, container, script, shell, env, stdout, stderr)
		return err
	}
	if len(shell) > 0 {
		_, err := runStepWithShellFn(ctx, shell, script, stdout, stderr, env, b.workDir)
		return err
	}
	_, err := runStepFn(ctx, script, stdout, stderr, env, b.workDir)
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
