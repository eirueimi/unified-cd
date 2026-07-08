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
	RunDefault(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error)
	RunNamedContainer(ctx context.Context, step api.ClaimStep, container, script string, env []string, stdout, stderr io.Writer) (int, error)

	EnsureScope(ctx context.Context, step api.ClaimStep, env []string) (ScopeHandle, error)
	RunInScope(ctx context.Context, h ScopeHandle, script string, env []string, stdout, stderr io.Writer) (int, error)
	CloseScopes(ctx context.Context)

	CacheRestore(ctx context.Context, scope ScopeHandle, key string, restoreKeys []string, path string) (bool, error)
	CacheSave(ctx context.Context, scope ScopeHandle, key, path string, ttlDays int) error
	UploadArtifact(ctx context.Context, scope ScopeHandle, runID, name, path string) error
	DownloadArtifact(ctx context.Context, scope ScopeHandle, runID, name, destDir string) error

	RunPostHook(ctx context.Context, scope ScopeHandle, container, script string, env []string) error

	// ResolveArtifactPath resolves a cache/artifact step's relative path (as
	// authored in the DSL) against the right root for scope: the claim's
	// non-scoped workspace root (host workDir / k8s pod mount path) when scope
	// is zero, or the scope container's fixed working directory
	// ("/workspace", the same value both agents use) when scope is non-zero.
	// An already-absolute p is returned unchanged. This is the one seam where
	// the shared orchestration loop defers to backend-specific knowledge (the
	// host uses OS-native path joining against an arbitrary host directory;
	// k8s uses forward-slash joining against a configurable pod mount path),
	// mirroring the pre-refactor host resolveWorkspacePath/resolveScopePath
	// pair and the k8s agent's inline path.Join(mountPath, ...).
	ResolveArtifactPath(scope ScopeHandle, p string) string

	// ResolveCachePath resolves a cache step's relative path for the
	// non-scoped case ONLY differently from ResolveArtifactPath: the host
	// agent deliberately leaves a non-scoped cache path unresolved (as
	// authored), since cache.Restore/cache.Save treat it as relative to the
	// objectstore's own root rather than the workspace directory, while the
	// k8s agent resolves it against the pod's mount path exactly like an
	// artifact path. The scoped case is identical to ResolveArtifactPath on
	// both backends (the scope container's fixed working directory).
	ResolveCachePath(scope ScopeHandle, p string) string

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
	// finish func called at step end. Flush/liveness semantics are backend-
	// specific and intentionally asymmetric: host returns LogPusher for both
	// streams (with StartAutoFlush bound to ctx); k8s returns its per-line
	// stdout logLineWriter and a LogPusher (auto-flushed) for stderr. The
	// {{ .Stdout }} capture buffer is the ORCHESTRATOR's concern — it tees
	// stdout via io.MultiWriter, so backends return shipping writers only.
	StepLogWriters(ctx context.Context, stepIndex int) (stdout, stderr io.Writer, finish func(ctx context.Context))

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
