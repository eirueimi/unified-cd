package agent

import (
	"context"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHostBackend_RunNamedContainer_ExecsIntoNamedContainer verifies that a
// runsIn.container step, when its container is defined in the claim's
// podTemplate, execs into a long-lived container bind-mounted at the host
// workspace — and that CloseScopes (claim end) tears it down. This replaces
// the former TestHostBackend_RunNamedContainer_NotSupported: runsIn.container
// is now supported on the host agent (see RunNamedContainer).
func TestHostBackend_RunNamedContainer_ExecsIntoNamedContainer(t *testing.T) {
	rt := &recordingRT{}
	a := &Agent{ID: "a1"}
	a.runtimeOnce.Do(func() {}) // mark runtime as resolved
	a.resolvedRuntime = rt

	pt := &dsl.PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"name": "tools", "image": "node:20"},
	}}}
	b := newHostBackend(a, "r1", "/host/ws", pt)

	step := api.ClaimStep{Index: 0, Name: "s", RunsIn: &dsl.RunsIn{Container: "tools"}}
	ec, err := b.RunNamedContainer(context.Background(), step, "tools", "echo hi", nil, nil, nil)
	if err != nil {
		t.Fatalf("RunNamedContainer: %v", err)
	}
	if ec != 0 {
		t.Fatalf("exit = %d, want 0", ec)
	}
	if got := rt.creates.Load(); got != 1 {
		t.Fatalf("expected 1 Create, got %d", got)
	}
	if len(rt.specs) != 1 || len(rt.specs[0].Mounts) != 1 || rt.specs[0].Mounts[0].HostPath != "/host/ws" {
		t.Fatalf("expected workspace bind mount from /host/ws, got %+v", rt.specs)
	}
	b.CloseScopes(context.Background())
	if got := rt.removes.Load(); got != 1 {
		t.Fatalf("expected teardown Remove, got %d", got)
	}
}

// TestHostBackend_RunNamedContainer_UnknownContainer verifies that a
// runsIn.container step whose container name is absent from the claim's
// podTemplate fails with a clear error rather than falling back to some
// other exec path.
func TestHostBackend_RunNamedContainer_UnknownContainer(t *testing.T) {
	rt := &recordingRT{}
	a := &Agent{ID: "a1"}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = rt
	pt := &dsl.PodTemplate{Spec: map[string]any{"containers": []any{}}}
	b := newHostBackend(a, "r1", "/host/ws", pt)

	step := api.ClaimStep{Name: "s", RunsIn: &dsl.RunsIn{Container: "missing"}}
	if _, err := b.RunNamedContainer(context.Background(), step, "missing", "echo hi", nil, nil, nil); err == nil {
		t.Fatal("expected error for a container not in the podTemplate")
	}
}

// TestHostBackend_ConcurrencyMode verifies the host agent reports Concurrent
// (its historical parallel-group/matrix execution behavior — goroutines, not
// sequential), matching the ConcurrencyMode contract executeRun relies on
// when driving RunPipeline.
func TestHostBackend_ConcurrencyMode(t *testing.T) {
	b := newHostBackend(&Agent{ID: "a1"}, "r1", t.TempDir(), nil)
	assert.Equal(t, Concurrent, b.ConcurrencyMode())
}

// TestHostBackend_StepLogWriters_FinishIsSafe verifies StepLogWriters returns
// usable writers and that calling finish is safe (stops the auto-flush
// goroutine and flushes both streams) even with no client traffic recorded —
// this is the thin unit-level lock on the log-plumbing move out of
// executeRun; end-to-end shipping behavior stays covered by the existing
// stdout-stream/parity suites.
func TestHostBackend_StepLogWriters_FinishIsSafe(t *testing.T) {
	b := newHostBackend(&Agent{ID: "a1", Client: NewClient("http://127.0.0.1:0", "tok")}, "r1", t.TempDir(), nil)
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
	b := newHostBackend(&Agent{ID: "a1", Client: NewClient("http://127.0.0.1:0", "tok")}, "r1", t.TempDir(), nil)
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
	b := newHostBackend(a, "r1", t.TempDir(), nil)
	destPath := t.TempDir()

	hit, err := b.CacheRestore(ctx, ScopeHandle{}, "nonexistent-key", nil, destPath)
	require.NoError(t, err, "cache miss should not return an error")
	assert.False(t, hit, "cache miss should report hit=false")
}
