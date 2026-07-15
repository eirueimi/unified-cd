package agent

import (
	"context"
	"io"
	"runtime"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func isolatedBackendForTest(t *testing.T) (*hostBackend, *podFakeRT) {
	t.Helper()
	f := &podFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "p", "r", "")
	require.NoError(t, m.Start(context.Background(), mysqlTemplate()))
	b := newHostBackend(&Agent{}, "run1", "/w", m)
	return b, f
}

func TestHostBackend_Isolated_RunDefaultExecsPrimary(t *testing.T) {
	b, f := isolatedBackendForTest(t)
	_, err := b.RunDefault(context.Background(), api.ClaimStep{Name: "s"}, "echo hi", nil, io.Discard, io.Discard)
	require.NoError(t, err)
	require.NotEmpty(t, f.execs)
	assert.Equal(t, "c2", f.execs[0].id) // injected "job" primary
}

func TestHostBackend_Isolated_RunNamedContainer(t *testing.T) {
	b, f := isolatedBackendForTest(t)
	_, err := b.RunNamedContainer(context.Background(), api.ClaimStep{}, "mysql", "echo hi", nil, io.Discard, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, "c1", f.execs[0].id)

	_, err = b.RunNamedContainer(context.Background(), api.ClaimStep{}, "nope", "x", nil, io.Discard, io.Discard)
	assert.Error(t, err)
}

func TestHostBackend_Isolated_DefaultAgentOSIsLinux(t *testing.T) {
	b, _ := isolatedBackendForTest(t)
	assert.Equal(t, "linux", b.DefaultAgentOS())
}

func TestHostBackend_Native_DefaultAgentOSIsHost(t *testing.T) {
	b := newHostBackend(&Agent{}, "run1", "/w", nil)
	assert.Equal(t, runtime.GOOS, b.DefaultAgentOS())
}

func TestHostBackend_Isolated_ResolveCachePathJoinsWorkDir(t *testing.T) {
	b, _ := isolatedBackendForTest(t)
	got, err := b.ResolveCachePath(ScopeHandle{}, "node_modules")
	require.NoError(t, err)
	want, err := resolveWorkspacePath("/w", "node_modules")
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestHostBackend_Native_ResolveCachePathJoinsWorkDir verifies the G1 fix: a
// non-scoped native (pod == nil) cache path now resolves against workDir
// exactly like an artifact path, instead of being left unresolved (the
// pre-fix behavior, which tarred the agent process's own CWD instead of the
// workspace).
func TestHostBackend_Native_ResolveCachePathJoinsWorkDir(t *testing.T) {
	b := newHostBackend(&Agent{}, "run1", "/w", nil)
	got, err := b.ResolveCachePath(ScopeHandle{}, "node_modules")
	require.NoError(t, err)
	want, err := resolveWorkspacePath("/w", "node_modules")
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestHostBackend_Isolated_WorkspacePathIsMountPath verifies that a
// non-scoped step on an isolated claim reports the pod's bind-mount path
// (not the host workDir) for UNIFIED_WORKSPACE, and that a scoped step still
// reports scopeWorkDir regardless of isolation.
func TestHostBackend_Isolated_WorkspacePathIsMountPath(t *testing.T) {
	b, _ := isolatedBackendForTest(t)
	assert.Equal(t, "/workspace", b.WorkspacePath(ScopeHandle{}))
	assert.Equal(t, scopeWorkDir, b.WorkspacePath(scopeHandleForTest()))
}

func TestHostBackend_Isolated_PostHookRunsInStepContainer(t *testing.T) {
	b, f := isolatedBackendForTest(t)
	require.NoError(t, b.RunPostHook(context.Background(), ScopeHandle{}, "mysql", "echo post", nil, nil, io.Discard, io.Discard))
	assert.Equal(t, "c1", f.execs[0].id)
	// container=="" post hook goes to the primary
	require.NoError(t, b.RunPostHook(context.Background(), ScopeHandle{}, "", "echo post2", nil, nil, io.Discard, io.Discard))
	assert.Equal(t, "c2", f.execs[1].id)
}

func TestHostBackend_Isolated_CloseScopesTearsDownPod(t *testing.T) {
	b, f := isolatedBackendForTest(t)
	b.CloseScopes(context.Background())
	assert.NotEmpty(t, f.removed)
}
