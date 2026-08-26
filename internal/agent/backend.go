package agent

import (
	"context"
	"io"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/secrets"
)

// ExecBackend is the narrow seam between the shared step-orchestration loop
// and a concrete execution environment (host process / k8s pod).
type ExecBackend interface {
	// RunDefault and RunNamedContainer receive the full step, so they can
	// read step.Shell (the controller-resolved effective interpreter argv,
	// nil meaning "apply the shim default") without a separate parameter.
	RunDefault(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error)
	RunNamedContainer(ctx context.Context, step api.ClaimStep, container, script string, env []string, stdout, stderr io.Writer) (int, error)

	EnsureScope(ctx context.Context, step api.ClaimStep, env []string) (ScopeHandle, error)
	// RunInScope executes script inside the scope container/pod identified
	// by h. shell is the owning step's effective interpreter argv
	// (api.ClaimStep.Shell — nil/empty means "apply the shim default"); the
	// orchestrator passes it explicitly since RunInScope, unlike
	// RunDefault/RunNamedContainer, does not otherwise receive the step.
	RunInScope(ctx context.Context, h ScopeHandle, script string, shell []string, env []string, stdout, stderr io.Writer) (int, error)
	// CloseScopes tears down everything the claim opened: scope
	// containers/Pods, the sidecar log pump, and (on the host backend) the
	// claim pod. It is called once, from RunClaim's teardown defer.
	//
	// CONTRACT ON ctx: it is the teardown phase's OWN budget window — a
	// context.WithTimeout over context.WithoutCancel(claim ctx), never a
	// cancelled one (see the defer in RunClaim). Implementations must
	// THREAD it through every call they make rather than re-stripping it
	// with context.WithoutCancel: that ceiling is the fourth of the four
	// documented cleanup windows (see DefaultFinallyBudget), and an
	// implementation that strips it turns the documented bound back into
	// "wedges forever on an unresponsive runtime/API server". Blocking
	// primitives that cannot take a context (a sync.WaitGroup join) must be
	// raced against ctx.Done() instead.
	CloseScopes(ctx context.Context)

	CacheRestore(ctx context.Context, scope ScopeHandle, key string, restoreKeys []string, path string) (bool, error)
	CacheSave(ctx context.Context, scope ScopeHandle, key, path string, ttlDays int) error
	UploadArtifact(ctx context.Context, scope ScopeHandle, runID, name, path string) error
	DownloadArtifact(ctx context.Context, scope ScopeHandle, runID, name, destDir string) error

	// RunPostHook runs a step's post: hook. shell is the hook's effective
	// interpreter argv: post.Shell if the post: hook declared its own, else
	// the owning step's effective ClaimStep.Shell (resolved by the
	// orchestrator into the hookStack entry — see postHookEntry.shell in
	// orchestrator.go); nil/empty means "apply the shim default", same as
	// every other exec path. stdout/stderr are the SHIPPING writers for the
	// owning step's log (see the hookStack drain in orchestrator.go: opened
	// via StepLogWriters against the owning step's index, same as a main
	// step's writers, so post output is masked and streamed identically) —
	// RunPostHook must feed the script's actual stdout/stderr into them, not
	// discard them.
	//
	// CONTRACT ON THE RETURN VALUE: post: hooks are best-effort cleanup, by
	// design, not a step that can fail the run. This method's signature
	// deliberately has no int exit code alongside the error (unlike
	// RunDefault/RunNamedContainer/RunInScope) — a non-zero exit is not
	// reported at all. The error return exists only so the orchestrator's
	// hookStack drain (orchestrator.go) can log a warning when the hook
	// failed to execute at all (couldn't spawn, exec stream broke, etc.); it
	// is never surfaced as a step or run failure, on either backend. This is
	// intentional and symmetric across the host and Kubernetes backends —
	// see docs/user-guide/writing-jobs/steps.md's "Post-step hooks" section
	// and docs/operator-manual/agents.md's cleanup-phase table ("A post:/
	// cache: hook has never changed the run's status, whatever it fails
	// on"). Do not "fix" this by threading the exit code through; that would
	// be a semantics change requiring its own decision, not a bug fix.
	RunPostHook(ctx context.Context, scope ScopeHandle, container, script string, shell []string, env []string, stdout, stderr io.Writer) error

	// ResolveArtifactPath resolves a cache/artifact step's relative path (as
	// authored in the DSL) against the right root for scope: the claim's
	// non-scoped workspace root (host workDir / k8s pod mount path) when scope
	// is zero, or the scope container's fixed working directory
	// ("/workspace", the same value both agents use) when scope is non-zero.
	// An absolute p, or a p that escapes the resolved root via "..", is
	// rejected as an error. This is the one seam where the shared
	// orchestration loop defers to backend-specific knowledge (the host uses
	// OS-native path joining against an arbitrary host directory; k8s uses
	// forward-slash joining against a configurable pod mount path), mirroring
	// the pre-refactor host resolveWorkspacePath/resolveScopePath pair and the
	// k8s agent's inline path.Join(mountPath, ...) — both now routed through
	// containment (ContainWithinOS / ContainWithinSlash).
	ResolveArtifactPath(scope ScopeHandle, p string) (string, error)

	// ResolveCachePath resolves a cache step's relative path identically to
	// ResolveArtifactPath on both backends: the scope container's fixed
	// working directory when scoped, and the claim's workspace root
	// (host workDir / k8s pod mount path) when non-scoped. An absolute p, or a
	// p that escapes the resolved root, is rejected as an error.
	ResolveCachePath(scope ScopeHandle, p string) (string, error)

	// WorkspacePath returns the cwd workspace root a step sees in this scope
	// (host workDir natively; the container mount path in isolated/k8s; the
	// scope container's cwd when scoped), exposed to steps as UNIFIED_WORKSPACE.
	WorkspacePath(scope ScopeHandle) string

	// DefaultAgentOS reports the OS a non-scoped, non-container: step
	// actually executes on, for the UNIFIED_AGENT_OS env var (scoped/
	// container: steps always report "linux" regardless of backend, since
	// they run in an isolated Linux container either way — see
	// agentOSForStep). This legitimately differs per backend: the host agent
	// executes such a step directly on its own OS (runtime.GOOS), while every
	// k8s exec path — including the "default pod" case — runs inside a Linux
	// pod, so k8sBackend always reports "linux".
	DefaultAgentOS() string

	// SetMasker installs the secret masker for all subsequently-created log
	// writers. Called once by the shared loop right after it fetches secrets
	// (the masker is born inside the loop, after backend construction).
	SetMasker(m *secrets.Masker)

	// StepLogWriters returns the SHIPPING writers for one step's output and a
	// finish func called at step end. Both production backends return a
	// LogPusher (batched, auto-flushed on the same flushCtx, flushed in
	// finish) for stdout AND stderr — symmetric, not asymmetric. k8s used to
	// give stdout a per-line writer instead (deleted; this comment used to
	// call that split "intentional", which is what let it stand uncaught
	// through a design document that assumed every backend batched both
	// streams). Do not reintroduce a per-line writer for either stream on
	// either backend: that reopens the two-DB-round-trips-per-line pattern
	// batched log ingestion (internal/store's AppendLogs) exists to remove.
	// The {{ .Stdout }} capture buffer is the ORCHESTRATOR's concern — it
	// tees stdout via io.MultiWriter, so backends return shipping writers
	// only.
	StepLogWriters(ctx context.Context, stepIndex int) (stdout, stderr io.Writer, finish func(ctx context.Context))

	// ConcurrencyMode reports how RunPipeline must run the members of a
	// parallel: group or a matrix:/foreach: expansion for this backend —
	// Concurrent (goroutines) or Sequential (declaration order, one at a
	// time). Read once per pipeline in RunClaim, so a backend must return a
	// constant for the life of a claim. Both production backends now report
	// Concurrent; Sequential exists for a backend whose per-claim state
	// cannot yet tolerate overlapping steps.
	ConcurrencyMode() ConcurrencyMode
}

// ScopeHandle is an opaque per-(ScopeID,MatrixKey) scope identity.
// Zero value = no scope / default location.
type ScopeHandle struct{ opaque any }

func (h ScopeHandle) IsZero() bool { return h.opaque == nil }

// NewScopeHandle wraps an arbitrary backend-specific payload as a
// ScopeHandle, so an ExecBackend implementation living in another package
// (e.g. the k8s agent) can construct one. A nil v yields the zero
// ScopeHandle. Pair with ScopeHandlePayload to recover the payload.
func NewScopeHandle(v any) ScopeHandle {
	if v == nil {
		return ScopeHandle{}
	}
	return ScopeHandle{opaque: v}
}

// ScopeHandlePayload returns the payload wrapped by NewScopeHandle. ok is
// false for the zero ScopeHandle.
func ScopeHandlePayload(h ScopeHandle) (v any, ok bool) {
	if h.IsZero() {
		return nil, false
	}
	return h.opaque, true
}
