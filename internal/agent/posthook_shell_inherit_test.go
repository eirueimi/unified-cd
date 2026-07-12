package agent

import (
	"context"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecuteRun_Isolated_PostHookInheritsStepShell is the end-to-end proof
// for the orchestrator's hookStack shell resolution (orchestrator.go's
// hookShell computation, appended onto postHookEntry.shell): a post: hook
// with no shell: of its own must inherit the owning step's effective
// ClaimStep.Shell, all the way through to the actual container exec.
func TestExecuteRun_Isolated_PostHookInheritsStepShell(t *testing.T) {
	const agentID = "posthook-inherit-agent"
	const runID = "run-posthook-inherit"

	h := newIsolatedHarness()
	srv := newIsolatedServer(t, agentID, h)

	f := &podFakeRT{}
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok"), PauseImage: "pause:img", RunnerImage: "runner:img"}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = f

	claim := api.ClaimResponse{
		Native:  false,
		RunID:   runID,
		JobName: "test-posthook-inherit",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, Name: "s1",
				Shell: []string{"bash", "-lc"},
				Run:   "echo hi",
				Post:  &api.PostStep{Run: "echo cleanup"}, // no Shell of its own -> inherit
			}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Equal(t, "Succeeded", h.finishStatus)

	require.Len(t, f.execSpecs, 2, "main step exec + post hook exec")
	assert.Equal(t, []string{"bash", "-lc"}, f.execSpecs[0].Shell, "main step exec uses its declared shell")
	assert.Equal(t, []string{"bash", "-lc"}, f.execSpecs[1].Shell, "post hook exec must inherit the owning step's effective shell")
}

// TestExecuteRun_Isolated_PostHookOwnShellOverridesStep is the companion
// case: a post: hook that declares its own shell: uses that argv instead of
// inheriting the owning step's — the override exists precisely so a
// non-shell interpreter step (e.g. shell: [python3, -c]) can still run a
// shell-script cleanup hook (spec Component 1, resolution priority notes).
func TestExecuteRun_Isolated_PostHookOwnShellOverridesStep(t *testing.T) {
	const agentID = "posthook-override-agent"
	const runID = "run-posthook-override"

	h := newIsolatedHarness()
	srv := newIsolatedServer(t, agentID, h)

	f := &podFakeRT{}
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok"), PauseImage: "pause:img", RunnerImage: "runner:img"}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = f

	claim := api.ClaimResponse{
		Native:  false,
		RunID:   runID,
		JobName: "test-posthook-override",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, Name: "s1",
				Shell: []string{"python3", "-c"},
				Run:   "print('hi')",
				Post:  &api.PostStep{Run: "echo cleanup", Shell: []string{"sh", "-c"}},
			}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Equal(t, "Succeeded", h.finishStatus)

	require.Len(t, f.execSpecs, 2)
	assert.Equal(t, []string{"python3", "-c"}, f.execSpecs[0].Shell, "main step exec uses its declared shell")
	assert.Equal(t, []string{"sh", "-c"}, f.execSpecs[1].Shell, "post hook's own declared shell must override, not inherit")
}

// TestExecuteRun_Isolated_PostHookNilShellResolvesToShimDefault covers the
// fully-unset case: neither the step nor its post: hook declares a shell,
// so the exec layer applies the shim default at both call sites.
func TestExecuteRun_Isolated_PostHookNilShellResolvesToShimDefault(t *testing.T) {
	const agentID = "posthook-default-agent"
	const runID = "run-posthook-default"

	h := newIsolatedHarness()
	srv := newIsolatedServer(t, agentID, h)

	f := &podFakeRT{}
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok"), PauseImage: "pause:img", RunnerImage: "runner:img"}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = f

	claim := api.ClaimResponse{
		Native:  false,
		RunID:   runID,
		JobName: "test-posthook-default",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, Name: "s1",
				Run:  "echo hi",
				Post: &api.PostStep{Run: "echo cleanup"},
			}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Equal(t, "Succeeded", h.finishStatus)

	require.Len(t, f.execSpecs, 2)
	assert.Equal(t, []string{"/.ucd/ucd-sh", "-c"}, f.execSpecs[0].Shell)
	assert.Equal(t, []string{"/.ucd/ucd-sh", "-c"}, f.execSpecs[1].Shell)
}
