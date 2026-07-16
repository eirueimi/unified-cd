package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestGCWorkspaces exercises gcWorkspaces directly (no Agent/network
// plumbing): an aged job dir is removed, a fresh one is kept, the
// dot-prefixed shim dir and wsBase itself are never touched, and a dir
// backing an active claim is protected even though it is old enough to
// otherwise qualify.
//
// Active-set key decision: `active` is keyed by the workspace directory's
// ABSOLUTE PATH (wsBase/working<slot>/<job>), not by run ID and not by a
// bare job name. The shared RunSet from Task 3 only holds run IDs, which
// cannot be mapped back to a job/workspace directory without extra
// bookkeeping the controller doesn't expose to the agent. A bare sanitized
// job name is also unsafe as a key: claimWorkDir is slot-scoped
// (working0/foo and working1/foo are two DIFFERENT directories), so two
// concurrent runs of the same job in different slots would collide under a
// job-name-only key and one of them could be GC'd out from under a live
// run. The full directory path is unambiguous and is exactly what the
// caller (agent.go) already computes per in-flight claim, so it is threaded
// straight into a workDir-keyed RunSet (see gcWorkspaces's doc comment).
func TestGCWorkspaces(t *testing.T) {
	base := t.TempDir()
	mk := func(rel string, age time.Duration) string {
		p := filepath.Join(base, rel)
		require.NoError(t, os.MkdirAll(p, 0o755))
		mt := time.Now().Add(-age)
		require.NoError(t, os.Chtimes(p, mt, mt))
		return p
	}
	old := mk("working0/oldjob", 10*24*time.Hour)
	fresh := mk("working0/freshjob", 1*time.Hour)
	activeDir := mk("working1/activejob", 30*24*time.Hour)
	tools := mk(".ucd-tools", 30*24*time.Hour)

	// activeDir's own absolute path is the active-set key.
	active := map[string]struct{}{activeDir: {}}

	removed, err := gcWorkspaces(base, 7*24*time.Hour, active, time.Now())
	require.NoError(t, err)

	require.DirExists(t, fresh, "fresh dir kept")
	require.DirExists(t, tools, "dot-prefixed shim dir never touched")
	require.DirExists(t, base, "wsBase itself never touched")
	require.DirExists(t, filepath.Join(base, "working0"), "working<slot> dir itself never touched")
	require.DirExists(t, filepath.Join(base, "working1"), "working<slot> dir itself never touched")
	require.NoDirExists(t, old, "aged dir removed")
	require.DirExists(t, activeDir, "active-run dir protected even though aged")

	require.ElementsMatch(t, []string{old}, removed, "removed should report exactly the aged, inactive dir")
}

// TestGCWorkspaces_NoWorkingDirs verifies gcWorkspaces is a safe no-op
// (no error, nothing removed) when wsBase has no working<slot> directories
// yet, e.g. a brand-new agent that hasn't claimed anything.
func TestGCWorkspaces_NoWorkingDirs(t *testing.T) {
	base := t.TempDir()
	removed, err := gcWorkspaces(base, 7*24*time.Hour, nil, time.Now())
	require.NoError(t, err)
	require.Empty(t, removed)
}

// TestGCWorkspaces_RetentionBoundary confirms the boundary is strictly
// "older than retention" (now.Sub(mtime) > retention), not >=: a dir aged
// exactly at the retention threshold is kept.
func TestGCWorkspaces_RetentionBoundary(t *testing.T) {
	base := t.TempDir()
	retention := 7 * 24 * time.Hour
	now := time.Now()

	boundary := filepath.Join(base, "working0", "boundaryjob")
	require.NoError(t, os.MkdirAll(boundary, 0o755))
	mt := now.Add(-retention)
	require.NoError(t, os.Chtimes(boundary, mt, mt))

	removed, err := gcWorkspaces(base, retention, nil, now)
	require.NoError(t, err)
	require.Empty(t, removed)
	require.DirExists(t, boundary)
}

// newGCTestServer returns an httptest.Server that satisfies everything
// Agent.Run needs for a brief run (register, an empty claim, and a
// catch-all 204 for reconcile/heartbeat/deregister/etc.), so these tests can
// drive the real Run() startup path rather than re-implementing its wiring.
func newGCTestServer(agentID string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", registerHandler)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/claim", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`)) //nolint:errcheck // empty claim response: no RunID
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return httptest.NewServer(mux)
}

// runAgentBriefly starts a.Run in the background, lets it run for settle,
// then cancels and joins it (with a generous timeout so a hang fails loudly
// instead of hanging the test suite).
func runAgentBriefly(t *testing.T, a *Agent, settle time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(settle)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Agent.Run did not return after cancel")
	}
}

// TestAgent_WorkspaceGC_DefaultDisabled is the default-disabled regression:
// with WorkspaceRetentionDays left at its zero value, a badly aged workspace
// dir must survive Run's startup sweep (because there is no sweep) —
// persistent workspaces are a feature until an operator explicitly opts in.
func TestAgent_WorkspaceGC_DefaultDisabled(t *testing.T) {
	srv := newGCTestServer("a-gc-disabled")
	defer srv.Close()

	wsDir := t.TempDir()
	agedDir := filepath.Join(wsDir, "working0", "oldjob")
	require.NoError(t, os.MkdirAll(agedDir, 0o755))
	old := time.Now().Add(-365 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(agedDir, old, old))

	a := &Agent{
		ID:            "a-gc-disabled",
		Client:        NewClient(srv.URL, "tok"),
		MaxConcurrent: 1,
		WorkspaceDir:  wsDir,
		// WorkspaceRetentionDays intentionally left at 0 (default/disabled).
	}
	runAgentBriefly(t, a, 200*time.Millisecond)

	require.DirExists(t, agedDir, "workspace GC must not run when WorkspaceRetentionDays is 0")
}

// TestAgent_WorkspaceGC_StartupSweep verifies that with
// WorkspaceRetentionDays > 0, Run's startup sweep (which fires once, after
// ReconcileRuns, before any claim can have populated activeWorkDirs) removes
// an aged, inactive workspace dir.
func TestAgent_WorkspaceGC_StartupSweep(t *testing.T) {
	srv := newGCTestServer("a-gc-enabled")
	defer srv.Close()

	wsDir := t.TempDir()
	agedDir := filepath.Join(wsDir, "working0", "oldjob")
	require.NoError(t, os.MkdirAll(agedDir, 0o755))
	old := time.Now().Add(-365 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(agedDir, old, old))

	a := &Agent{
		ID:                     "a-gc-enabled",
		Client:                 NewClient(srv.URL, "tok"),
		MaxConcurrent:          1,
		WorkspaceDir:           wsDir,
		WorkspaceRetentionDays: 7,
	}
	runAgentBriefly(t, a, 200*time.Millisecond)

	require.NoDirExists(t, agedDir, "startup sweep must remove the aged workspace dir when WorkspaceRetentionDays > 0")
}
