package k8sagent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strconv"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/secrets"
)

// k8sBackend is the ExecBackend implementation for the k8s agent. It owns the
// per-claim pod identity (the pooled/per-run pod's name and workspace mount
// path), the claim's scope-pod map (lazily created on first uses-scope step,
// mirroring the doc comment on executeRun's old scopePods map — the k8s agent
// runs a claim's steps one at a time via agentlib.RunPipeline in Sequential
// mode, so this map needs no mutex for reads/writes performed from the
// orchestrate loop itself), and the secret masker used by StepLogWriters.
type k8sBackend struct {
	a         *K8sAgent
	runID     string
	podName   string
	mountPath string

	scopePods map[string]string

	masker *secrets.Masker
}

// newK8sBackend constructs the ExecBackend for one claim's executeRun call,
// after the run/pooled pod has been acquired and is Running.
func newK8sBackend(a *K8sAgent, runID, podName, mountPath string) *k8sBackend {
	return &k8sBackend{a: a, runID: runID, podName: podName, mountPath: mountPath, scopePods: map[string]string{}}
}

// RunDefault runs a step in the default (pooled/per-run) pod's container.
func (b *k8sBackend) RunDefault(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return b.a.exec.ExecStep(ctx, b.podName, execContainer(step), script, env, stdout, stderr)
}

// RunImage runs a step in a fresh, throwaway pod built from step.RunsIn.Image.
func (b *k8sBackend) RunImage(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error) {
	envMap := make(map[string]string, len(step.Env)+1)
	for k, v := range step.Env {
		envMap[k] = v
	}
	envMap["UNIFIED_AGENT_OS"] = "linux"
	deadline := imageStepDeadline(step)
	return b.a.runImageStep(ctx, b.runID, step.RunsIn.Image, envMap, deadline, step.RunsIn.Resources, script, stdout, stderr)
}

// RunNamedContainer runs a step inside a specific named container of the
// default pod (runsIn.container), via the same ExecStep path as RunDefault.
func (b *k8sBackend) RunNamedContainer(ctx context.Context, step api.ClaimStep, container, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return b.a.exec.ExecStep(ctx, b.podName, container, script, env, stdout, stderr)
}

// EnsureScope provisions (or reuses) the step's uses-scope pod, returning a
// ScopeHandle whose opaque payload is the scope pod's name.
func (b *k8sBackend) EnsureScope(ctx context.Context, step api.ClaimStep, env []string) (agentlib.ScopeHandle, error) {
	name, err := b.ensureScopePod(ctx, step)
	if err != nil {
		return agentlib.ScopeHandle{}, err
	}
	return wrapK8sScope(name), nil
}

// RunInScope execs script into the scope pod's "step" container.
func (b *k8sBackend) RunInScope(ctx context.Context, h agentlib.ScopeHandle, script string, env []string, stdout, stderr io.Writer) (int, error) {
	podName, ok := unwrapK8sScope(h)
	if !ok {
		return -1, fmt.Errorf("RunInScope: no scope handle")
	}
	return b.a.exec.ExecStep(ctx, podName, "step", script, env, stdout, stderr)
}

// CloseScopes deletes every scope pod opened during the claim.
func (b *k8sBackend) CloseScopes(ctx context.Context) {
	for key, name := range b.scopePods {
		if err := b.a.pm.DeletePod(context.WithoutCancel(ctx), name); err != nil {
			slog.Warn("k8s: failed to delete scope pod", "scopeKey", key, "pod", name, "error", err)
		}
	}
}

// ensureScopePod lazily creates (or returns the cached) scope pod for a scoped
// step, keyed by scopeKey. See executeRun's historical doc comment: this map
// is only ever touched from orchestrate's single-goroutine, Sequential-mode
// step loop, so no mutex is needed.
func (b *k8sBackend) ensureScopePod(ctx context.Context, step api.ClaimStep) (string, error) {
	key := scopeKey(step)
	if name, ok := b.scopePods[key]; ok {
		return name, nil
	}
	env := imageStepEnv(step)
	pod := buildScopePod(b.runID, b.a.cfg.Namespace, step.ScopeID, step.ScopeImage, env,
		SidecarSpec{Image: b.a.cfg.SidecarImage, S3SecretName: b.a.cfg.SidecarS3SecretName})
	created, err := b.a.pm.CreatePod(ctx, pod)
	if err != nil {
		return "", fmt.Errorf("uses-scope %q (image %q): create pod: %w", step.ScopeID, step.ScopeImage, err)
	}
	name := created.Name
	waitCtx, cancel := context.WithTimeout(ctx, imagePodStartTimeout)
	defer cancel()
	if err := b.a.pm.WaitForPodRunning(waitCtx, name); err != nil {
		// Best-effort cleanup of the pod that never became ready; CloseScopes
		// also sweeps b.scopePods, but this one never made it into the map.
		_ = b.a.pm.DeletePod(context.WithoutCancel(ctx), name)
		return "", fmt.Errorf("uses-scope %q (image %q): pod did not become ready within %s: %w", step.ScopeID, step.ScopeImage, imagePodStartTimeout, err)
	}
	b.scopePods[key] = name
	return name, nil
}

// resolveSidecarTarget returns the sidecar container name, workspace mount
// path, and target pod name (empty = default pod) for a cache/artifact
// operation against scope. A scoped operation targets its scope pod's private
// scratch volume instead of the run pod's shared workspace.
func (b *k8sBackend) resolveSidecarTarget(ctx context.Context, scope agentlib.ScopeHandle) (sidecar, mount, targetPod string, err error) {
	if scope.IsZero() {
		return artifactSidecarName, b.mountPath, "", nil
	}
	podName, ok := unwrapK8sScope(scope)
	if !ok {
		return "", "", "", fmt.Errorf("resolveSidecarTarget: invalid scope handle")
	}
	return artifactSidecarName, scopeMountPath, podName, nil
}

// CacheRestore execs the unified-sidecar binary's "cache restore" into the
// target pod's sidecar. Best-effort: a miss/error is reported back to the
// caller via (false, err) but callers treat cache as lenient.
func (b *k8sBackend) CacheRestore(ctx context.Context, scope agentlib.ScopeHandle, key string, restoreKeys []string, path string) (bool, error) {
	sidecar, _, targetPod, err := b.resolveSidecarTarget(ctx, scope)
	if err != nil {
		return false, err
	}
	argv := []string{"unified-sidecar", "cache", "restore", "--key", key, "--path", path}
	for _, rk := range restoreKeys {
		argv = append(argv, "--restore-key", rk)
	}
	ec, err := b.sidecarExecArgv(ctx, targetPod, sidecar, argv)
	if err != nil {
		return false, err
	}
	// The sidecar binary reports a cache hit via exit code 0 either way (a
	// miss is not distinguishable from a hit at this layer; orchestrate logs
	// "restore attempted" rather than a true hit/miss bool), so any successful
	// exec of the restore command is reported as a "hit" attempt (true) to
	// match the historical best-effort semantics: the caller never fails the
	// step on a cache miss regardless of this return value.
	return ec == 0, nil
}

// CacheSave execs the unified-sidecar binary's "cache save" into the target
// pod's sidecar.
func (b *k8sBackend) CacheSave(ctx context.Context, scope agentlib.ScopeHandle, key, path string, ttlDays int) error {
	sidecar, _, targetPod, err := b.resolveSidecarTarget(ctx, scope)
	if err != nil {
		return err
	}
	argv := []string{"unified-sidecar", "cache", "save", "--key", key, "--ttl-days", strconv.Itoa(ttlDays), "--path", path}
	_, err = b.sidecarExecArgv(ctx, targetPod, sidecar, argv)
	return err
}

// UploadArtifact execs the unified-sidecar binary's "artifact upload" into
// the target pod's sidecar.
func (b *k8sBackend) UploadArtifact(ctx context.Context, scope agentlib.ScopeHandle, runID, name, path string) error {
	sidecar, _, targetPod, err := b.resolveSidecarTarget(ctx, scope)
	if err != nil {
		return err
	}
	argv := []string{"unified-sidecar", "artifact", "upload", "--run", runID, "--name", name, "--path", path}
	ec, err := b.sidecarExecArgv(ctx, targetPod, sidecar, argv)
	if err != nil {
		return err
	}
	if ec != 0 {
		return fmt.Errorf("artifact upload %q: sidecar exited %d", name, ec)
	}
	return nil
}

// DownloadArtifact execs the unified-sidecar binary's "artifact download"
// into the target pod's sidecar.
func (b *k8sBackend) DownloadArtifact(ctx context.Context, scope agentlib.ScopeHandle, runID, name, destDir string) error {
	sidecar, _, targetPod, err := b.resolveSidecarTarget(ctx, scope)
	if err != nil {
		return err
	}
	argv := []string{"unified-sidecar", "artifact", "download", "--run", runID, "--name", name, "--dest", destDir}
	ec, err := b.sidecarExecArgv(ctx, targetPod, sidecar, argv)
	if err != nil {
		return err
	}
	if ec != 0 {
		return fmt.Errorf("artifact download %q: sidecar exited %d", name, ec)
	}
	return nil
}

// sidecarExecArgv execs argv (no shell) into the sidecar container of
// targetPod (empty means the default pooled/run pod), shipping stderr via a
// LogPusher on stepIndex 0 (mirroring the pre-refactor sidecarExec closure —
// cache/artifact steps have no per-step log stream of their own).
func (b *k8sBackend) sidecarExecArgv(ctx context.Context, targetPod, container string, argv []string) (int, error) {
	if targetPod == "" {
		targetPod = b.podName
	}
	stderrPusher := agentlib.NewLogPusher(b.a.client, b.a.cfg.AgentID, b.runID, 0, "stderr")
	stderrPusher.SetMasker(b.masker)
	ec, err := b.a.exec.ExecStepArgv(ctx, targetPod, container, argv, io.Discard, stderrPusher)
	stderrPusher.Flush(ctx)
	return ec, err
}

// ResolveArtifactPath resolves p against the run/pooled pod's workspace mount
// path (non-scoped) or the scope pod's fixed working directory (scoped),
// mirroring the pre-refactor orchestrate's inline path.Join(mountPath, ...) /
// path.Join(scopeMountPath, ...) computation.
func (b *k8sBackend) ResolveArtifactPath(scope agentlib.ScopeHandle, p string) string {
	if !scope.IsZero() {
		return path.Join(scopeMountPath, p)
	}
	return path.Join(b.mountPath, p)
}

// ResolveCachePath is identical to ResolveArtifactPath on k8s: a non-scoped
// cache path is resolved against the pod's mount path exactly like an
// artifact path (unlike the host agent, which leaves it unresolved).
func (b *k8sBackend) ResolveCachePath(scope agentlib.ScopeHandle, p string) string {
	return b.ResolveArtifactPath(scope, p)
}

// DefaultAgentOS always reports "linux": every k8s exec path — including the
// "default pod" case — runs inside a Linux pod, unlike the host agent, which
// executes a non-scoped, non-runsIn.image step directly on its own OS.
func (b *k8sBackend) DefaultAgentOS() string {
	return "linux"
}

// RunPostHook runs a post: hook's script in scope's pod (the "step"
// container) when scope is non-zero, else in targetPod/container (the
// default pod's routing decided by the caller — container is meaningful on
// k8s, unlike the host backend, since a post hook must run in the same
// container the step body ran in).
func (b *k8sBackend) RunPostHook(ctx context.Context, scope agentlib.ScopeHandle, container, script string, env []string) error {
	targetPod := ""
	if !scope.IsZero() {
		podName, ok := unwrapK8sScope(scope)
		if !ok {
			return fmt.Errorf("RunPostHook: invalid scope handle")
		}
		targetPod = podName
		container = "step"
	}
	if targetPod == "" {
		targetPod = b.podName
	}
	_, err := b.a.exec.ExecStep(ctx, targetPod, container, script, env, io.Discard, io.Discard)
	return err
}

// SetMasker installs the secret masker used by subsequently-created log
// writers (see StepLogWriters) and sidecar-exec stderr shipping.
func (b *k8sBackend) SetMasker(m *secrets.Masker) {
	b.masker = m
}

// StepLogWriters returns a per-line logLineWriter for stdout and a
// LogPusher (auto-flushed for the step's duration) for stderr, mirroring the
// pre-refactor stepExec closure. finish stops the auto-flush goroutine and
// does a final Flush of stderr.
func (b *k8sBackend) StepLogWriters(ctx context.Context, stepIndex int) (stdout, stderr io.Writer, finish func(ctx context.Context)) {
	stderrPusher := agentlib.NewLogPusher(b.a.client, b.a.cfg.AgentID, b.runID, stepIndex, "stderr")
	stderrPusher.SetMasker(b.masker)
	stdoutWriter := &logLineWriter{
		client: b.a.client, agentID: b.a.cfg.AgentID, runID: b.runID, stepIdx: stepIndex, stream: "stdout",
		masker: b.masker,
	}

	flushCtx, stopAutoFlush := context.WithCancel(ctx)
	stderrPusher.StartAutoFlush(flushCtx, stderrAutoFlushInterval)

	finish = func(finishCtx context.Context) {
		stopAutoFlush()
		stderrPusher.Flush(finishCtx)
	}
	return stdoutWriter, stderrPusher, finish
}

// ConcurrencyMode reports how the k8s agent runs parallel-group / matrix
// members: one at a time (its documented behavior — scope-pod map and hook
// stack are not concurrency-safe).
func (b *k8sBackend) ConcurrencyMode() agentlib.ConcurrencyMode {
	return agentlib.Sequential
}

var _ agentlib.ExecBackend = (*k8sBackend)(nil)

// wrapK8sScope wraps a scope pod name as a ScopeHandle. An empty name yields
// the zero ScopeHandle (no scope / default location).
func wrapK8sScope(podName string) agentlib.ScopeHandle {
	if podName == "" {
		return agentlib.ScopeHandle{}
	}
	return agentlib.NewScopeHandle(podName)
}

// unwrapK8sScope extracts the scope pod name from a ScopeHandle produced by
// wrapK8sScope.
func unwrapK8sScope(h agentlib.ScopeHandle) (string, bool) {
	v, ok := agentlib.ScopeHandlePayload(h)
	if !ok {
		return "", false
	}
	name, ok := v.(string)
	return name, ok
}
