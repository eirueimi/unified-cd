package agent

import (
	"context"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHostBackend_RunNamedContainer_NotSupported verifies that runsIn.container
// is rejected on the host agent with a clear, actionable error — the host has
// no long-lived named-container / sidecar model (that is the k8s agent's
// job), so this must never silently fall back to some other exec path.
func TestHostBackend_RunNamedContainer_NotSupported(t *testing.T) {
	b := newHostBackend(&Agent{ID: "a1"}, "r1", t.TempDir())
	_, err := b.RunNamedContainer(context.Background(), api.ClaimStep{Name: "s"}, "sidecar", "echo hi", nil, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported on the host agent")
}

// TestHostBackend_ConcurrencyMode verifies the host agent reports Concurrent
// (its historical parallel-group/matrix execution behavior — goroutines, not
// sequential), matching the ConcurrencyMode contract executeRun relies on
// when driving RunPipeline.
func TestHostBackend_ConcurrencyMode(t *testing.T) {
	b := newHostBackend(&Agent{ID: "a1"}, "r1", t.TempDir())
	assert.Equal(t, Concurrent, b.ConcurrencyMode())
}

// TestHostBackend_StepLogWriters_FinishIsSafe verifies StepLogWriters returns
// usable writers and that calling finish is safe (stops the auto-flush
// goroutine and flushes both streams) even with no client traffic recorded —
// this is the thin unit-level lock on the log-plumbing move out of
// executeRun; end-to-end shipping behavior stays covered by the existing
// stdout-stream/parity suites.
func TestHostBackend_StepLogWriters_FinishIsSafe(t *testing.T) {
	b := newHostBackend(&Agent{ID: "a1", Client: NewClient("http://127.0.0.1:0", "tok")}, "r1", t.TempDir())
	b.SetMasker(secrets.NoOpMasker)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdout, stderr, finish := b.StepLogWriters(ctx, 0)
	require.NotNil(t, stdout)
	require.NotNil(t, stderr)
	require.NotNil(t, finish)

	n, err := stdout.Write([]byte("hello stdout\n"))
	require.NoError(t, err)
	assert.Equal(t, len("hello stdout\n"), n)

	n, err = stderr.Write([]byte("hello stderr\n"))
	require.NoError(t, err)
	assert.Equal(t, len("hello stderr\n"), n)

	// Must not panic/hang even though the fake client endpoint refuses
	// connections; Flush's own retry/backoff is bounded (see LogPusher.Flush).
	finish(context.Background())
}

// TestHostBackend_StepLogWriters_ReturnsLogPushers verifies the host backend's
// StepLogWriters returns *LogPusher for both streams (per the ExecBackend
// StepLogWriters doc comment: "host returns LogPusher for both streams").
func TestHostBackend_StepLogWriters_ReturnsLogPushers(t *testing.T) {
	b := newHostBackend(&Agent{ID: "a1", Client: NewClient("http://127.0.0.1:0", "tok")}, "r1", t.TempDir())
	b.SetMasker(secrets.NoOpMasker)

	stdout, stderr, _ := b.StepLogWriters(context.Background(), 0)
	_, ok := stdout.(*LogPusher)
	assert.True(t, ok, "stdout writer must be a *LogPusher on the host backend")
	_, ok = stderr.(*LogPusher)
	assert.True(t, ok, "stderr writer must be a *LogPusher on the host backend")
}

// TestHostBackend_CacheRestore_MissReturnsNil verifies that a non-scoped
// CacheRestore against an empty cache returns (false, nil) on a miss, not an
// error — matching the scoped-branch behavior.
func TestHostBackend_CacheRestore_MissReturnsNil(t *testing.T) {
	ctx := context.Background()
	a := newCacheTestAgent(t)
	b := newHostBackend(a, "r1", t.TempDir())
	destPath := t.TempDir()

	hit, err := b.CacheRestore(ctx, ScopeHandle{}, "nonexistent-key", nil, destPath)
	require.NoError(t, err, "cache miss should not return an error")
	assert.False(t, hit, "cache miss should report hit=false")
}
