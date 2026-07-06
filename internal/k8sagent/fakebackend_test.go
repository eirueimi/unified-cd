package k8sagent

import (
	"context"
	"io"
	"strconv"
	"sync"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/secrets"
)

// fakeStep describes what a fake step exec should return, keyed by step name.
type fakeStep struct {
	exit   int
	stdout string
}

// artifactCall records a single call to the fake backend's cache/artifact
// sidecar dispatch (CacheRestore/CacheSave/UploadArtifact/DownloadArtifact),
// reconstructed as the argv the pre-refactor sidecarExec closure would have
// received — this keeps every existing argv-shaped assertion in
// orchestrate_test.go intact without the orchestrate loop itself dealing in
// argv anymore (that is now internal to the k8sBackend/fakeK8sBackend).
type artifactCall struct {
	targetPod string
	container string
	argv      []string
}

// postExecCall records a single call to the fake backend's RunPostHook.
type postExecCall struct {
	targetPod, container, script string
	env                          []string
}

// fakeK8sBackend is the single shared ExecBackend fake for every
// orchestrate_*_test.go / callstep_test.go file: it stands in for the
// pre-refactor stepExec/sidecarExec/postExec/ensureScopePod closure bundle,
// consolidated into one struct per the Task 7 brief. Each test configures
// only the fields/hooks it needs; zero-valued fields behave as harmless
// no-ops (0 exit code, no recorded calls).
type fakeK8sBackend struct {
	mu sync.Mutex

	// Fakes keyed by step name for the default run path (RunDefault/RunImage/
	// RunNamedContainer/RunInScope all consult this map identically — none of
	// the existing tests exercise more than one of these paths per step).
	Fakes map[string]fakeStep

	// StepExecFn, when set, overrides Fakes entirely and is invoked for every
	// exec path (RunDefault/RunImage/RunNamedContainer/RunInScope), receiving
	// the execCtx passed in — used by the cancel/timeout tests to observe or
	// block on ctx.
	StepExecFn func(ctx context.Context, step api.ClaimStep, script string) (exitCode int, err error)

	// EnsureScopeFn returns the scope pod name for a scoped step; defaults to
	// "scope-pod-"+step.ScopeID, matching every pre-refactor fakeEnsureScopePod.
	EnsureScopeFn func(step api.ClaimStep) (string, error)

	// SidecarExitCode is returned by every CacheRestore/CacheSave/
	// UploadArtifact/DownloadArtifact call (0 = success). Mirrors the
	// pre-refactor runOrchestrateArtifact's exitCode parameter.
	SidecarExitCode int
	// SidecarErr, when non-nil, is returned by every sidecar dispatch call
	// instead of translating SidecarExitCode.
	SidecarErr error

	// PostExecFn, when set, is invoked for every RunPostHook call instead of
	// the default no-op-success behavior.
	PostExecFn func(ctx context.Context, targetPod, container, script string, env []string) error

	ArtifactCalls []artifactCall
	PostCalls     []postExecCall
}

func newFakeK8sBackend() *fakeK8sBackend {
	return &fakeK8sBackend{}
}

func (f *fakeK8sBackend) recordArtifactCall(targetPod, container string, argv []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ArtifactCalls = append(f.ArtifactCalls, artifactCall{targetPod: targetPod, container: container, argv: argv})
}

func (f *fakeK8sBackend) recordPostCall(c postExecCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PostCalls = append(f.PostCalls, c)
}

// runFake is the shared implementation behind RunDefault/RunImage/
// RunNamedContainer/RunInScope: it dispatches to StepExecFn if set, else
// looks the step up in Fakes (default: exit 0, no output).
func (f *fakeK8sBackend) runFake(ctx context.Context, step api.ClaimStep, script string, stdout, stderr io.Writer) (int, error) {
	if f.StepExecFn != nil {
		ec, err := f.StepExecFn(ctx, step, script)
		return ec, err
	}
	fk, ok := f.Fakes[step.Name]
	if !ok {
		return 0, nil
	}
	if fk.stdout != "" && stdout != nil {
		_, _ = stdout.Write([]byte(fk.stdout))
	}
	return fk.exit, nil
}

func (f *fakeK8sBackend) RunDefault(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return f.runFake(ctx, step, script, stdout, stderr)
}

func (f *fakeK8sBackend) RunImage(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return f.runFake(ctx, step, script, stdout, stderr)
}

func (f *fakeK8sBackend) RunNamedContainer(ctx context.Context, step api.ClaimStep, container, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return f.runFake(ctx, step, script, stdout, stderr)
}

func (f *fakeK8sBackend) EnsureScope(ctx context.Context, step api.ClaimStep, env []string) (agentlib.ScopeHandle, error) {
	if f.EnsureScopeFn != nil {
		name, err := f.EnsureScopeFn(step)
		if err != nil {
			return agentlib.ScopeHandle{}, err
		}
		if name == "" {
			return agentlib.ScopeHandle{}, nil
		}
		return agentlib.NewScopeHandle(name), nil
	}
	name := "scope-pod-" + step.ScopeID
	return agentlib.NewScopeHandle(name), nil
}

func (f *fakeK8sBackend) RunInScope(ctx context.Context, h agentlib.ScopeHandle, script string, env []string, stdout, stderr io.Writer) (int, error) {
	step := api.ClaimStep{}
	if v, ok := agentlib.ScopeHandlePayload(h); ok {
		if name, ok := v.(string); ok {
			step.ScopeID = name
		}
	}
	return f.runFake(ctx, step, script, stdout, stderr)
}

func (f *fakeK8sBackend) CloseScopes(ctx context.Context) {}

func (f *fakeK8sBackend) scopePodName(scope agentlib.ScopeHandle) string {
	if v, ok := agentlib.ScopeHandlePayload(scope); ok {
		if name, ok := v.(string); ok {
			return name
		}
	}
	return ""
}

func (f *fakeK8sBackend) sidecarResult() (int, error) {
	if f.SidecarErr != nil {
		return f.SidecarExitCode, f.SidecarErr
	}
	return f.SidecarExitCode, nil
}

func (f *fakeK8sBackend) CacheRestore(ctx context.Context, scope agentlib.ScopeHandle, key string, restoreKeys []string, path string) (bool, error) {
	argv := []string{"unified-sidecar", "cache", "restore", "--key", key, "--path", path}
	for _, rk := range restoreKeys {
		argv = append(argv, "--restore-key", rk)
	}
	f.recordArtifactCall(f.scopePodName(scope), artifactSidecarName, argv)
	ec, err := f.sidecarResult()
	if err != nil {
		return false, err
	}
	return ec == 0, nil
}

func (f *fakeK8sBackend) CacheSave(ctx context.Context, scope agentlib.ScopeHandle, key, path string, ttlDays int) error {
	argv := []string{"unified-sidecar", "cache", "save", "--key", key, "--ttl-days", strconv.Itoa(ttlDays), "--path", path}
	f.recordArtifactCall(f.scopePodName(scope), artifactSidecarName, argv)
	_, err := f.sidecarResult()
	return err
}

func (f *fakeK8sBackend) UploadArtifact(ctx context.Context, scope agentlib.ScopeHandle, runID, name, path string) error {
	argv := []string{"unified-sidecar", "artifact", "upload", "--run", runID, "--name", name, "--path", path}
	f.recordArtifactCall(f.scopePodName(scope), artifactSidecarName, argv)
	ec, err := f.sidecarResult()
	if err != nil {
		return err
	}
	if ec != 0 {
		return errExitNonZero
	}
	return nil
}

func (f *fakeK8sBackend) DownloadArtifact(ctx context.Context, scope agentlib.ScopeHandle, runID, name, destDir string) error {
	argv := []string{"unified-sidecar", "artifact", "download", "--run", runID, "--name", name, "--dest", destDir}
	f.recordArtifactCall(f.scopePodName(scope), artifactSidecarName, argv)
	ec, err := f.sidecarResult()
	if err != nil {
		return err
	}
	if ec != 0 {
		return errExitNonZero
	}
	return nil
}

func (f *fakeK8sBackend) RunPostHook(ctx context.Context, scope agentlib.ScopeHandle, container, script string, env []string) error {
	targetPod := f.scopePodName(scope)
	if targetPod != "" {
		container = "step"
	}
	f.recordPostCall(postExecCall{targetPod: targetPod, container: container, script: script, env: env})
	if f.PostExecFn != nil {
		return f.PostExecFn(ctx, targetPod, container, script, env)
	}
	return nil
}

func (f *fakeK8sBackend) SetMasker(m *secrets.Masker) {}

func (f *fakeK8sBackend) StepLogWriters(ctx context.Context, stepIndex int) (stdout, stderr io.Writer, finish func(ctx context.Context)) {
	return io.Discard, io.Discard, func(context.Context) {}
}

func (f *fakeK8sBackend) ConcurrencyMode() agentlib.ConcurrencyMode {
	return agentlib.Sequential
}

var _ agentlib.ExecBackend = (*fakeK8sBackend)(nil)

// errExitNonZero is a sentinel error for a fake sidecar exec that "exited"
// with a non-zero code, mirroring the pre-refactor artifact branches'
// err != nil || ec != 0 => Failed check.
var errExitNonZero = &nonZeroExitError{}

type nonZeroExitError struct{}

func (e *nonZeroExitError) Error() string { return "sidecar exited non-zero" }
