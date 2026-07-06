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
	RunImage(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error)
	RunNamedContainer(ctx context.Context, step api.ClaimStep, container, script string, env []string, stdout, stderr io.Writer) (int, error)

	EnsureScope(ctx context.Context, step api.ClaimStep, env []string) (ScopeHandle, error)
	RunInScope(ctx context.Context, h ScopeHandle, script string, env []string, stdout, stderr io.Writer) (int, error)
	CloseScopes(ctx context.Context)

	CacheRestore(ctx context.Context, scope ScopeHandle, key string, restoreKeys []string, path string) (bool, error)
	CacheSave(ctx context.Context, scope ScopeHandle, key, path string, ttlDays int) error
	UploadArtifact(ctx context.Context, scope ScopeHandle, runID, name, path string) error
	DownloadArtifact(ctx context.Context, scope ScopeHandle, runID, name, destDir string) error

	RunPostHook(ctx context.Context, scope ScopeHandle, container, script string, env []string) error

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
