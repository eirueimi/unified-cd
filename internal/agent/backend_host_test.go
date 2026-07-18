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

// TestHostBackend_RunNamedContainer_ExecsIntoPodContainer verifies that a
// container: step on an isolated claim execs into the claim pod's named
// container (bind-mounted at the workspace) and that CloseScopes (claim end)
// tears the pod down. This is the container: dispatch case, now routed through
// the claim pod that replaced the retired namedContainerManager.
func TestHostBackend_RunNamedContainer_ExecsIntoPodContainer(t *testing.T) {
	rt := &recordingRT{}
	pt := &dsl.PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"name": "tools", "image": "node:20"},
	}}}
	pod := newClaimPodManager(rt, "/host/ws", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, pod.Start(context.Background(), pt))
	b := newHostBackend(&Agent{ID: "a1"}, "r1", "test-job", "/host/ws", pod)

	step := api.ClaimStep{Index: 0, Name: "s", Container: "tools"}
	ec, err := b.RunNamedContainer(context.Background(), step, "tools", "echo hi", nil, nil, nil)
	if err != nil {
		t.Fatalf("RunNamedContainer: %v", err)
	}
	if ec != 0 {
		t.Fatalf("exit = %d, want 0", ec)
	}
	// pause + tools + injected "job" = 3 containers, all bind-mounting the workspace.
	if got := rt.creates.Load(); got != 3 {
		t.Fatalf("expected 3 Create (pause + tools + job), got %d", got)
	}
	toolsSpec := rt.specs[1] // pause is [0]
	if len(toolsSpec.Mounts) != 1 || toolsSpec.Mounts[0].HostPath != "/host/ws" {
		t.Fatalf("expected workspace bind mount from /host/ws, got %+v", toolsSpec)
	}
	b.CloseScopes(context.Background())
	if got := rt.removes.Load(); got != 3 {
		t.Fatalf("expected pod teardown (3 Remove), got %d", got)
	}
}

// TestHostBackend_RunNamedContainer_UnknownContainer verifies that a
// container: step whose name is absent from the claim pod fails with a clear
// error rather than falling back to some other exec path.
func TestHostBackend_RunNamedContainer_UnknownContainer(t *testing.T) {
	rt := &recordingRT{}
	pt := &dsl.PodTemplate{Spec: map[string]any{"containers": []any{}}}
	pod := newClaimPodManager(rt, "/host/ws", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, pod.Start(context.Background(), pt))
	b := newHostBackend(&Agent{ID: "a1"}, "r1", "test-job", "/host/ws", pod)

	step := api.ClaimStep{Name: "s", Container: "missing"}
	if _, err := b.RunNamedContainer(context.Background(), step, "missing", "echo hi", nil, nil, nil); err == nil {
		t.Fatal("expected error for a container not in the claim pod")
	}
}

// TestHostBackend_Native_RunNamedContainer_Errors verifies a native claim
// (nil pod) rejects a container: step rather than silently running it on the
// host.
func TestHostBackend_Native_RunNamedContainer_Errors(t *testing.T) {
	b := newHostBackend(&Agent{ID: "a1"}, "r1", "test-job", "/host/ws", nil)
	step := api.ClaimStep{Name: "s", Container: "tools"}
	if _, err := b.RunNamedContainer(context.Background(), step, "tools", "echo hi", nil, nil, nil); err == nil {
		t.Fatal("expected error: container: step on a native claim")
	}
}

// TestHostBackend_ConcurrencyMode verifies the host agent reports Concurrent
// (its historical parallel-group/matrix execution behavior — goroutines, not
// sequential), matching the ConcurrencyMode contract executeRun relies on
// when driving RunPipeline.
func TestHostBackend_ConcurrencyMode(t *testing.T) {
	b := newHostBackend(&Agent{ID: "a1"}, "r1", "test-job", t.TempDir(), nil)
	assert.Equal(t, Concurrent, b.ConcurrencyMode())
}

// TestHostBackend_StepLogWriters_FinishIsSafe verifies StepLogWriters returns
// usable writers and that calling finish is safe (stops the auto-flush
// goroutine and flushes both streams) even with no client traffic recorded —
// this is the thin unit-level lock on the log-plumbing move out of
// executeRun; end-to-end shipping behavior stays covered by the existing
// stdout-stream/parity suites.
func TestHostBackend_StepLogWriters_FinishIsSafe(t *testing.T) {
	b := newHostBackend(&Agent{ID: "a1", Client: NewClient("http://127.0.0.1:0", "tok")}, "r1", "test-job", t.TempDir(), nil)
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
	b := newHostBackend(&Agent{ID: "a1", Client: NewClient("http://127.0.0.1:0", "tok")}, "r1", "test-job", t.TempDir(), nil)
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
	b := newHostBackend(a, "r1", "test-job", t.TempDir(), nil)
	destPath := t.TempDir()

	hit, err := b.CacheRestore(ctx, ScopeHandle{}, "nonexistent-key", nil, destPath)
	require.NoError(t, err, "cache miss should not return an error")
	assert.False(t, hit, "cache miss should report hit=false")
}

// TestHostResolve_ContainmentAndG1 proves two things about the native
// (pod == nil) host backend at once: (1) G1 — a non-scoped native CACHE path
// now resolves against workDir exactly like an artifact path, instead of
// being left unresolved (the pre-fix behavior, which tarred the agent
// process's own CWD instead of the workspace); and (2) F-PATH-1 — a
// traversal path ("../..") is rejected with a containment error for both
// resolvers, not silently joined outside the workspace.
func TestHostResolve_ContainmentAndG1(t *testing.T) {
	// native backend: pod == nil
	b := &hostBackend{workDir: "/tmp/ws"}

	// G1: a non-scoped native CACHE path now resolves against workDir
	// (previously returned unresolved).
	got, err := b.ResolveCachePath(ScopeHandle{}, "node_modules")
	require.NoError(t, err)
	assert.Equal(t, filepathJoin("/tmp/ws", "node_modules"), got)

	// artifact path resolves the same way
	got, err = b.ResolveArtifactPath(ScopeHandle{}, "dist")
	require.NoError(t, err)
	assert.Equal(t, filepathJoin("/tmp/ws", "dist"), got)

	// containment: traversal rejected for both
	_, err = b.ResolveArtifactPath(ScopeHandle{}, "../../etc/passwd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes the workspace")
	_, err = b.ResolveCachePath(ScopeHandle{}, "../../etc/passwd")
	require.Error(t, err)
}

// scopeHandleForTest builds a non-zero ScopeHandle via the same exported
// constructor the orchestrator uses (NewScopeHandle) — WorkspacePath only
// inspects IsZero(), never the payload, so the wrapped value is arbitrary.
func scopeHandleForTest() ScopeHandle {
	return NewScopeHandle("test-scope")
}

// TestHostWorkspacePath verifies UNIFIED_WORKSPACE's native/scoped values on
// the host backend: native (pod == nil) reports workDir, and a scoped step
// always reports scopeWorkDir regardless of native/isolated. The isolated
// (pod != nil, mountPath) case is covered by
// TestHostBackend_Isolated_WorkspacePathIsMountPath in backend_isolated_test.go.
func TestHostWorkspacePath(t *testing.T) {
	native := &hostBackend{workDir: "/tmp/ws"}
	assert.Equal(t, "/tmp/ws", native.WorkspacePath(ScopeHandle{}))
	// scoped is always the container cwd
	assert.Equal(t, scopeWorkDir, native.WorkspacePath(scopeHandleForTest()))
}
