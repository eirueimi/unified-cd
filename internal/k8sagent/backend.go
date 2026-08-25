package k8sagent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/secrets"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// scopeEntry is one scope key's in-flight or completed pod creation. The Once
// makes concurrent callers for the same key share a single attempt instead of
// each creating a pod — the old check-then-act let two steps both miss the
// cache, and the loser's pod was orphaned, since CloseScopes only deletes
// pods that made it into the map.
//
// name and err are written inside the Once and read by CloseScopes, which
// never calls Do and so has no happens-before edge from it. Both accesses are
// therefore taken under k8sBackend.scopesMu.
type scopeEntry struct {
	once sync.Once
	name string
	err  error
}

// k8sBackend is the ExecBackend implementation for the k8s agent. It owns the
// per-claim pod identity (the pooled/per-run pod's name and workspace mount
// path), the claim's scope-pod entries (lazily created on first uses-scope
// step; scopesMu guards scopes so concurrent steps sharing a uses-scope can
// safely race to create it — see scopeEntry), and the secret masker used by
// StepLogWriters.
type k8sBackend struct {
	a       *K8sAgent
	runID   string
	podName string
	// jobName is the claim's qualified job name (e.g. "team-a/build"), passed
	// as the sidecar's --job flag on every cache restore/save exec so cache
	// entries stay namespaced per job (mirrors hostBackend.jobName).
	jobName   string
	mountPath string

	scopesMu sync.Mutex
	scopes   map[string]*scopeEntry

	masker *secrets.Masker

	// sidecarNames/claimSince/sidecarPump drive user-sidecar log streaming.
	// The pump is constructed (and started) lazily in SetMasker, once the
	// masker is known, and stopped in CloseScopes — mirroring the host
	// backend's pump ownership model (see backend_host.go). claimSince bounds
	// GetLogs' replay to this run's window so a reused pooled pod doesn't
	// re-emit a previous claim's sidecar output.
	sidecarNames []string
	claimSince   metav1.Time
	sidecarPump  *k8sSidecarPump
}

// newK8sBackend constructs the ExecBackend for one claim's executeRun call,
// after the run/pooled pod has been acquired and is Running. sidecarNames are
// the user podTemplate sidecar container names (declared order); claimSince is
// the claim's start time, used as the sidecar log stream's SinceTime. jobName
// is the claim's qualified job name, passed to the sidecar's cache
// restore/save subcommands via --job for per-job cache namespacing.
func newK8sBackend(a *K8sAgent, runID, jobName, podName, mountPath string, sidecarNames []string, claimSince metav1.Time) *k8sBackend {
	return &k8sBackend{
		a: a, runID: runID, jobName: jobName, podName: podName, mountPath: mountPath,
		scopes:       map[string]*scopeEntry{},
		sidecarNames: sidecarNames, claimSince: claimSince,
	}
}

// RunDefault runs a step in the default (pooled/per-run) pod's container,
// threading step.Shell (the controller-resolved effective interpreter argv;
// nil means "apply the shim default") through to Executor.ExecStep.
func (b *k8sBackend) RunDefault(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return b.a.exec.ExecStep(ctx, b.podName, execContainer(step), script, step.Shell, env, stdout, stderr)
}

// envSliceToMap converts "KEY=VALUE" pairs (as produced by the orchestrator's
// already-template-expanded extraEnv) into a map. A malformed entry with no
// "=" is skipped defensively; the orchestrator never produces one.
func envSliceToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

// RunNamedContainer runs a step inside a specific named container of the
// default pod (runsIn.container), via the same ExecStep path as RunDefault,
// threading step.Shell through identically.
func (b *k8sBackend) RunNamedContainer(ctx context.Context, step api.ClaimStep, container, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return b.a.exec.ExecStep(ctx, b.podName, container, script, step.Shell, env, stdout, stderr)
}

// EnsureScope provisions (or reuses) the step's uses-scope pod, returning a
// ScopeHandle whose opaque payload is the scope pod's name.
func (b *k8sBackend) EnsureScope(ctx context.Context, step api.ClaimStep, env []string) (agentlib.ScopeHandle, error) {
	name, err := b.ensureScopePod(ctx, step, env)
	if err != nil {
		return agentlib.ScopeHandle{}, err
	}
	return wrapK8sScope(name), nil
}

// RunInScope execs script into the scope pod's "step" container.
//
// shell is the owning step's effective interpreter argv (agentlib.ExecBackend
// interface addition for the step-shell-shim feature; nil/empty means "apply
// the shim default"), threaded through to Executor.ExecStep exactly like
// RunDefault/RunNamedContainer thread step.Shell.
func (b *k8sBackend) RunInScope(ctx context.Context, h agentlib.ScopeHandle, script string, shell []string, env []string, stdout, stderr io.Writer) (int, error) {
	podName, ok := unwrapK8sScope(h)
	if !ok {
		return -1, fmt.Errorf("RunInScope: no scope handle")
	}
	return b.a.exec.ExecStep(ctx, podName, "step", script, shell, env, stdout, stderr)
}

// CloseScopes deletes every scope pod opened during the claim. It first stops
// the sidecar log pump (cancelling all sidecar streams and flushing remainders)
// so streams end before the pod is deleted/released — RunClaim defers
// CloseScopes and returns before agent.go's pod-teardown defer fires.
//
// It claims each entry out of b.scopes (deletes it from the map) before
// deleting its pod, rather than just reading name/err under the lock and
// deleting afterward. That claim is an ownership handoff with an in-flight
// ensureScopePod/createScopePod for the same key: e.name is written (in
// createScopePod) before e.err is known, so a CloseScopes racing with an
// in-flight attempt could otherwise see e.err == nil (its zero value, not yet
// set) and e.name != "" for a pod that is still waiting on
// WaitForPodRunning, or has just failed but whose once.Do closure has not yet
// recorded e.err, and issue its own DeletePod for the same pod
// createScopePod's own failure branch also deletes. Removing the entry here
// first means createScopePod's failure branch (which checks whether its key
// still maps to its own entry before deleting) sees it is already gone and
// skips its delete.
//
// In production this race cannot actually happen: CloseScopes is deferred in
// the orchestrator and only runs after RunPipeline returns, and runParallel
// joins its goroutines before returning, so no in-flight ensureScopePod can
// overlap CloseScopes. That ordering lives in a different package
// (internal/agent/orchestrator.go) though, so this handoff does not rely on
// it — it stays correct even if a future refactor there changes it.
func (b *k8sBackend) CloseScopes(ctx context.Context) {
	if b.sidecarPump != nil {
		b.sidecarPump.Stop()
	}

	b.scopesMu.Lock()
	entries := make(map[string]string, len(b.scopes))
	for key, e := range b.scopes {
		if e.err == nil && e.name != "" {
			entries[key] = e.name
			delete(b.scopes, key)
		}
	}
	b.scopesMu.Unlock()

	for key, name := range entries {
		if err := b.a.pm.DeletePod(context.WithoutCancel(ctx), name); err != nil {
			slog.Warn("k8s: failed to delete scope pod", "scopeKey", key, "pod", name, "error", err)
		}
	}
}

// ensureScopePod lazily creates (or returns the in-flight/completed) scope
// pod for a scoped step, keyed by scopeKey. Concurrent callers for the same
// key share a single scopeEntry and therefore a single sync.Once, so exactly
// one of them runs createScopePod; the rest block on Do until it finishes and
// then read the same result. Different keys get different entries and so
// proceed independently — scopesMu only ever guards the map/entry bookkeeping,
// never the pod creation itself.
func (b *k8sBackend) ensureScopePod(ctx context.Context, step api.ClaimStep, env []string) (string, error) {
	key := scopeKey(step)

	b.scopesMu.Lock()
	e, ok := b.scopes[key]
	if !ok {
		e = &scopeEntry{}
		b.scopes[key] = e
	}
	b.scopesMu.Unlock()

	e.once.Do(func() {
		name, err := b.createScopePod(ctx, step, env, e)
		b.scopesMu.Lock()
		e.name, e.err = name, err
		if err != nil {
			// Do not cache a failure. A later step needing this scope makes
			// its own attempt rather than inheriting an error it did not
			// cause; the callers waiting on THIS attempt still receive err
			// below, because they hold the entry pointer. This delete is
			// idempotent with createScopePod's own ownership-claiming delete
			// on its WaitForPodRunning failure branch, and with CloseScopes'
			// claim (see its doc comment) — whichever of the three removes
			// the entry first, the rest are no-ops.
			delete(b.scopes, key)
		}
		b.scopesMu.Unlock()
	})

	b.scopesMu.Lock()
	name, err := e.name, e.err
	b.scopesMu.Unlock()
	return name, err
}

// createScopePod creates one scope pod and waits for it to be Running. It is
// called at most once per scope key, from inside scopeEntry.once.
//
// The scope pod's Ready wait is bounded by the same configurable knob as the
// run pod (Config.PodStartTimeout / UNIFIED_K8S_POD_START_TIMEOUT, resolved
// via Config.PodStartTimeoutDuration — see config.go and agent.go's
// awaitPodRunning). Under RestartPolicy: Never a pod stuck in
// Pending/ImagePullBackOff never transitions to Failed, so without this bound
// the wait would hang until the whole run is cancelled; this gives a bad
// scope image the same fast, explicit failure the run pod gets.
//
// The scope pod's env mirrors the expanded-env fix (commit 9e09c76): the
// orchestrator's already-template-expanded env (KEY=VALUE pairs) is merged
// over imageStepEnv(step)'s k8s-specific defaults, so the caller's expanded
// value wins over the raw, unexpanded step.Env map and a templated env value
// (e.g. {{ .Params.x }}) ships resolved rather than as the literal template.
func (b *k8sBackend) createScopePod(ctx context.Context, step api.ClaimStep, env []string, e *scopeEntry) (string, error) {
	key := scopeKey(step)
	envMap := imageStepEnv(step)
	for k, v := range envSliceToMap(env) {
		envMap[k] = v
	}
	pod := buildScopePod(b.runID, b.a.cfg.Namespace, step.ScopeID, step.ScopeImage, envMap,
		SidecarSpec{Image: b.a.cfg.SidecarImage, S3SecretName: b.a.cfg.SidecarS3SecretName}, b.a.cfg.ShimImage)
	created, err := b.a.pm.CreatePod(ctx, pod)
	if err != nil {
		return "", fmt.Errorf("uses-scope %q (image %q): create pod: %w", step.ScopeID, step.ScopeImage, err)
	}
	name := created.Name
	// Record the name as soon as the Pod exists, before waiting for Running,
	// so CloseScopes can always name what exists. From here until this
	// function returns, a real Pod is running in the cluster; if we only
	// wrote e.name on success (as ensureScopePod's caller does at the end),
	// a CloseScopes racing in while we are still in WaitForPodRunning below
	// would see an entry with no name and skip it, leaking the Pod.
	b.scopesMu.Lock()
	e.name = name
	b.scopesMu.Unlock()
	podStartTimeout := b.a.cfg.PodStartTimeoutDuration()
	waitCtx, cancel := context.WithTimeout(ctx, podStartTimeout)
	defer cancel()
	if err := b.a.pm.WaitForPodRunning(waitCtx, name); err != nil {
		// Best-effort cleanup of the pod that never became ready. e.name was
		// already recorded above, but ensureScopePod deletes this whole entry
		// from b.scopes on failure (see the err != nil branch there), so a
		// CloseScopes that runs after we return would no longer find it
		// either — normally we must delete it ourselves here.
		//
		// "Normally" because of an ownership check first: a CloseScopes
		// racing with this attempt (see its doc comment) may have already
		// claimed this key out of b.scopes and taken responsibility for
		// deleting the pod. Only delete here if our entry is still the one in
		// the map; otherwise CloseScopes has it, and a delete here would be
		// redundant (and could race the fake/API client with CloseScopes' own
		// delete).
		b.scopesMu.Lock()
		owned := b.scopes[key] == e
		if owned {
			delete(b.scopes, key)
		}
		b.scopesMu.Unlock()
		if owned {
			_ = b.a.pm.DeletePod(context.WithoutCancel(ctx), name)
		}
		return "", fmt.Errorf("uses-scope %q (image %q): pod did not become ready within %s: %w", step.ScopeID, step.ScopeImage, podStartTimeout, err)
	}
	return name, nil
}

// resolveSidecarTarget returns the sidecar container name and target pod name
// (empty = default pod) for a cache/artifact operation against scope. A
// scoped operation targets its scope pod's private scratch volume instead of
// the run pod's shared workspace; the mount path itself is resolved
// separately via ResolveArtifactPath/ResolveCachePath, so it is not part of
// this return.
func (b *k8sBackend) resolveSidecarTarget(ctx context.Context, scope agentlib.ScopeHandle) (sidecar, targetPod string, err error) {
	if scope.IsZero() {
		return artifactSidecarName, "", nil
	}
	podName, ok := unwrapK8sScope(scope)
	if !ok {
		return "", "", fmt.Errorf("resolveSidecarTarget: invalid scope handle")
	}
	return artifactSidecarName, podName, nil
}

// CacheRestore execs the unified-sidecar binary's "cache restore" into the
// target pod's sidecar. Best-effort: a miss/error is reported back to the
// caller via (false, err) but callers treat cache as lenient.
func (b *k8sBackend) CacheRestore(ctx context.Context, scope agentlib.ScopeHandle, key string, restoreKeys []string, path string) (bool, error) {
	sidecar, targetPod, err := b.resolveSidecarTarget(ctx, scope)
	if err != nil {
		return false, err
	}
	argv := []string{"unified-sidecar", "cache", "restore", "--key", key, "--path", path, "--job", b.jobName}
	for _, rk := range restoreKeys {
		argv = append(argv, "--restore-key", rk)
	}
	ec, stdout, err := b.sidecarExecArgvCapturingStdout(ctx, targetPod, sidecar, argv)
	if err != nil {
		return false, err
	}
	// exit code stays best-effort (0 on hit/miss/error); the true hit/miss comes
	// from the sidecar's UCD_CACHE_RESULT stdout marker so the orchestrator logs
	// an accurate hit/miss (parity with the host's ErrCacheMiss-based bool).
	_ = ec
	return parseCacheResult(stdout), nil
}

// parseCacheResult reads the UCD_CACHE_RESULT marker from the sidecar's stdout.
// Absent marker (older sidecar, or the error path that emits none) defaults to a
// hit, preserving the historical lenient best-effort behavior.
func parseCacheResult(stdout string) bool {
	return !strings.Contains(stdout, "UCD_CACHE_RESULT=miss")
}

// CacheSave execs the unified-sidecar binary's "cache save" into the target
// pod's sidecar.
func (b *k8sBackend) CacheSave(ctx context.Context, scope agentlib.ScopeHandle, key, path string, ttlDays int) error {
	sidecar, targetPod, err := b.resolveSidecarTarget(ctx, scope)
	if err != nil {
		return err
	}
	argv := []string{"unified-sidecar", "cache", "save", "--key", key, "--ttl-days", strconv.Itoa(ttlDays), "--path", path, "--job", b.jobName}
	_, err = b.sidecarExecArgv(ctx, targetPod, sidecar, argv)
	return err
}

// UploadArtifact execs the unified-sidecar binary's "artifact upload" into
// the target pod's sidecar.
func (b *k8sBackend) UploadArtifact(ctx context.Context, scope agentlib.ScopeHandle, runID, name, path string) error {
	sidecar, targetPod, err := b.resolveSidecarTarget(ctx, scope)
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
	sidecar, targetPod, err := b.resolveSidecarTarget(ctx, scope)
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
// LogPusher on the artifact sidecar's dsl.ArtifactLogIndex (its own identity,
// not step 0's stream) (mirroring the pre-refactor sidecarExec closure —
// cache/artifact steps have no per-step log stream of their own).
func (b *k8sBackend) sidecarExecArgv(ctx context.Context, targetPod, container string, argv []string) (int, error) {
	if targetPod == "" {
		targetPod = b.podName
	}
	stderrPusher := agentlib.NewLogPusher(b.a.client, b.a.cfg.AgentID, b.runID, dsl.ArtifactLogIndex, "stderr")
	stderrPusher.SetMasker(b.masker)
	ec, err := b.a.exec.ExecStepArgv(ctx, targetPod, container, argv, io.Discard, stderrPusher)
	stderrPusher.Flush(ctx)
	return ec, err
}

// sidecarExecArgvCapturingStdout is sidecarExecArgv but returns the sidecar's
// stdout (still shipping stderr to the log pusher). Used by CacheRestore to read
// the UCD_CACHE_RESULT marker.
func (b *k8sBackend) sidecarExecArgvCapturingStdout(ctx context.Context, targetPod, container string, argv []string) (int, string, error) {
	if targetPod == "" {
		targetPod = b.podName
	}
	stderrPusher := agentlib.NewLogPusher(b.a.client, b.a.cfg.AgentID, b.runID, dsl.ArtifactLogIndex, "stderr")
	stderrPusher.SetMasker(b.masker)
	var stdout bytes.Buffer
	ec, err := b.a.exec.ExecStepArgv(ctx, targetPod, container, argv, &stdout, stderrPusher)
	stderrPusher.Flush(ctx)
	return ec, stdout.String(), err
}

// ResolveArtifactPath resolves p against the run/pooled pod's workspace mount
// path (non-scoped) or the scope pod's fixed working directory (scoped),
// mirroring the pre-refactor orchestrate's inline path.Join(mountPath, ...) /
// path.Join(scopeMountPath, ...) computation — now routed through
// agentlib.ContainWithinSlash so an absolute or escaping p is rejected
// instead of reaching outside the mount (e.g. the sidecar's mounted secrets).
func (b *k8sBackend) ResolveArtifactPath(scope agentlib.ScopeHandle, p string) (string, error) {
	if !scope.IsZero() {
		return agentlib.ContainWithinSlash(scopeMountPath, p)
	}
	return agentlib.ContainWithinSlash(b.mountPath, p)
}

// ResolveCachePath is identical to ResolveArtifactPath on k8s: a non-scoped
// cache path is resolved against the pod's mount path exactly like an
// artifact path (unlike the pre-fix host agent, which left it unresolved).
func (b *k8sBackend) ResolveCachePath(scope agentlib.ScopeHandle, p string) (string, error) {
	return b.ResolveArtifactPath(scope, p)
}

// WorkspacePath reports the cwd workspace root a step sees on this backend,
// for UNIFIED_WORKSPACE: the scope pod's fixed working directory when scope
// is non-zero, else the run/pooled pod's workspace mount path.
func (b *k8sBackend) WorkspacePath(scope agentlib.ScopeHandle) string {
	if !scope.IsZero() {
		return scopeMountPath
	}
	return b.mountPath
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
// container the step body ran in). stdout/stderr are the owning step's
// shipping writers (see agentlib.ExecBackend.RunPostHook's doc comment); they
// replace what used to be a hardcoded io.Discard, io.Discard pair that threw
// post-hook output away.
//
// shell is the hook's effective interpreter argv (agentlib.ExecBackend
// interface addition for the step-shell-shim feature: post.Shell if the
// post: hook declared its own, else the owning step's effective
// ClaimStep.Shell — already resolved into this parameter by the
// orchestrator's hookStack, see orchestrator.go's hookShell computation),
// threaded through to Executor.ExecStep exactly like every other exec path.
func (b *k8sBackend) RunPostHook(ctx context.Context, scope agentlib.ScopeHandle, container, script string, shell []string, env []string, stdout, stderr io.Writer) error {
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
	_, err := b.a.exec.ExecStep(ctx, targetPod, container, script, shell, env, stdout, stderr)
	return err
}

// SetMasker installs the secret masker used by subsequently-created log
// writers (see StepLogWriters) and sidecar-exec stderr shipping. It also starts
// the user-sidecar log pump now that the masker is known, so sidecar logs are
// masked like every other stream (mirrors the host backend). GetLogs replays
// from claimSince regardless of when Start runs, so starting here — after
// secrets fetch — loses no history. Best-effort: if the underlying podManager
// is not a *PodManager (e.g. a test fake), streaming is skipped with a warning.
func (b *k8sBackend) SetMasker(m *secrets.Masker) {
	b.masker = m
	if len(b.sidecarNames) == 0 {
		return
	}
	pm, ok := b.a.pm.(*PodManager)
	if !ok {
		slog.Warn("k8s: sidecar log streaming skipped: podManager is not *PodManager", "runId", b.runID)
		return
	}
	b.sidecarPump = &k8sSidecarPump{
		client: pm.Client(), logs: b.a.client, ns: b.a.cfg.Namespace,
		pod: b.podName, agentID: b.a.cfg.AgentID, runID: b.runID,
		masker: b.masker, sidecars: b.sidecarNames, since: b.claimSince,
	}
	// context.Background(): the streams must outlive per-step cancellation;
	// Stop (called from CloseScopes) cancels them at claim end.
	b.sidecarPump.Start(context.Background())
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
// members: concurrently, matching the standard agent.
//
// This returned Sequential until scope-pod creation became concurrency-safe.
// The old comment also blamed the hook stack, which was already wrong: the
// hook stack and postHooks live in the shared orchestrator
// (internal/agent/orchestrator.go) and have been guarded by postHooksMu since
// the standard agent went concurrent. Everything else the backend holds is
// either written once before the step loop (masker, sidecarPump, via
// SetMasker) or allocated per call (StepLogWriters), and the pod pool carries
// its own mutex.
func (b *k8sBackend) ConcurrencyMode() agentlib.ConcurrencyMode {
	return agentlib.Concurrent
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
