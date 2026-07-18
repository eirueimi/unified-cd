package agent

import (
	"context"
	"io"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withFakeRunStepFns swaps runStepFn/runStepWithShellFn (backend_host.go's
// exec seams) for recording fakes, so native-path dispatch (nil step.Shell
// -> RunStep, non-nil -> RunStepWithShell with the declared argv) can be
// asserted without spawning a real host process. Restored via t.Cleanup.
func withFakeRunStepFns(t *testing.T) (plainCalls *int, shellCalls *[][]string) {
	t.Helper()
	origPlain, origShell := runStepFn, runStepWithShellFn
	pc := 0
	var sc [][]string
	runStepFn = func(ctx context.Context, script string, stdout, stderr io.Writer, extraEnv []string, exposeEnv []string, workDir string) (int, error) {
		pc++
		return 0, nil
	}
	runStepWithShellFn = func(ctx context.Context, shell []string, script string, stdout, stderr io.Writer, extraEnv []string, exposeEnv []string, workDir string) (int, error) {
		sc = append(sc, append([]string{}, shell...))
		return 0, nil
	}
	t.Cleanup(func() { runStepFn, runStepWithShellFn = origPlain, origShell })
	return &pc, &sc
}

// TestHostBackend_Native_RunDefault_NilShellUsesRunStep verifies a native
// claim's default step with no ClaimStep.Shell keeps today's unconditional
// host-bash path (RunStep), unchanged.
func TestHostBackend_Native_RunDefault_NilShellUsesRunStep(t *testing.T) {
	plainCalls, shellCalls := withFakeRunStepFns(t)
	b := newHostBackend(&Agent{}, "run1", "/w", nil)

	_, err := b.RunDefault(context.Background(), api.ClaimStep{Name: "s"}, "echo hi", nil, io.Discard, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 1, *plainCalls, "nil step.Shell must dispatch to RunStep")
	assert.Empty(t, *shellCalls)
}

// TestHostBackend_Native_RunDefault_ExplicitShellUsesRunStepWithShell
// verifies a native claim's default step with a declared ClaimStep.Shell
// execs that argv + [script] as a host process (RunStepWithShell) instead of
// the unconditional bash path.
func TestHostBackend_Native_RunDefault_ExplicitShellUsesRunStepWithShell(t *testing.T) {
	plainCalls, shellCalls := withFakeRunStepFns(t)
	b := newHostBackend(&Agent{}, "run1", "/w", nil)

	step := api.ClaimStep{Name: "s", Shell: []string{"python3", "-c"}}
	_, err := b.RunDefault(context.Background(), step, "print('hi')", nil, io.Discard, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 0, *plainCalls, "an explicit step.Shell must not fall through to RunStep")
	require.Len(t, *shellCalls, 1)
	assert.Equal(t, []string{"python3", "-c"}, (*shellCalls)[0])
}

// TestHostBackend_Native_RunPostHook_InheritsAndOverridesShell exercises
// RunPostHook's native-process branch directly (the orchestrator resolves
// inherit-vs-override into the shell parameter before calling RunPostHook —
// see orchestrator.go's hookShell computation and
// TestExecuteRun_Isolated_PostHookInheritsStepShell below for the end-to-end
// resolution proof); this test locks in RunPostHook's own dispatch given
// whatever shell it is handed.
func TestHostBackend_Native_RunPostHook_InheritsAndOverridesShell(t *testing.T) {
	plainCalls, shellCalls := withFakeRunStepFns(t)
	b := newHostBackend(&Agent{}, "run1", "/w", nil)

	// nil shell (post declared none AND the step had none) -> RunStep.
	require.NoError(t, b.RunPostHook(context.Background(), ScopeHandle{}, "", "cleanup", nil, nil, io.Discard, io.Discard))
	assert.Equal(t, 1, *plainCalls)
	assert.Empty(t, *shellCalls)

	// explicit shell (inherited or overridden, RunPostHook doesn't care which) -> RunStepWithShell.
	require.NoError(t, b.RunPostHook(context.Background(), ScopeHandle{}, "", "cleanup", []string{"sh", "-c"}, nil, io.Discard, io.Discard))
	assert.Equal(t, 1, *plainCalls, "the explicit-shell call must not also hit RunStep")
	require.Len(t, *shellCalls, 1)
	assert.Equal(t, []string{"sh", "-c"}, (*shellCalls)[0])
}

// TestHostBackend_Isolated_RunDefault_ThreadsStepShell verifies the claim-pod
// path passes step.Shell through to the container exec (via
// claimPodManager.Exec -> effectiveShell), both for the unset (shim default)
// and explicit cases.
func TestHostBackend_Isolated_RunDefault_ThreadsStepShell(t *testing.T) {
	b, f := isolatedBackendForTest(t)

	_, err := b.RunDefault(context.Background(), api.ClaimStep{Name: "s"}, "echo hi", nil, io.Discard, io.Discard)
	require.NoError(t, err)
	require.NotEmpty(t, f.execSpecs)
	assert.Equal(t, []string{"/.ucd/ucd-sh", "-c"}, f.execSpecs[len(f.execSpecs)-1].Shell,
		"nil step.Shell must resolve to the shim default at the exec layer")

	step := api.ClaimStep{Name: "s2", Shell: []string{"bash", "-lc"}}
	_, err = b.RunDefault(context.Background(), step, "echo hi", nil, io.Discard, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, []string{"bash", "-lc"}, f.execSpecs[len(f.execSpecs)-1].Shell)
}

// TestHostBackend_Isolated_RunNamedContainer_ThreadsStepShell mirrors
// RunDefault's threading test for the container: dispatch path.
func TestHostBackend_Isolated_RunNamedContainer_ThreadsStepShell(t *testing.T) {
	b, f := isolatedBackendForTest(t)

	step := api.ClaimStep{Name: "s", Shell: []string{"bash", "-lc"}}
	_, err := b.RunNamedContainer(context.Background(), step, "mysql", "echo hi", nil, io.Discard, io.Discard)
	require.NoError(t, err)
	require.NotEmpty(t, f.execSpecs)
	assert.Equal(t, []string{"bash", "-lc"}, f.execSpecs[len(f.execSpecs)-1].Shell)
}

// TestHostBackend_Isolated_RunInScope_ThreadsShell verifies RunInScope
// threads the shell argv the caller passes (the orchestrator supplies
// step.Shell — see orchestrator.go's RunInScope call) through to the scope
// container's exec.
func TestHostBackend_Isolated_RunInScope_ThreadsShell(t *testing.T) {
	f := &fakeRT{}
	sm := newScopeManager(f, "")
	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "img"}
	h, err := sm.ensure(context.Background(), step, nil)
	require.NoError(t, err)

	b := newHostBackend(&Agent{}, "run1", "/w", nil)
	b.scopes = sm
	handle := wrapHostScope(sm, h)

	_, err = b.RunInScope(context.Background(), handle, "echo hi", nil, nil, io.Discard, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, []string{"/.ucd/ucd-sh", "-c"}, f.lastExecSpec.Shell, "nil shell must resolve to the shim default")

	_, err = b.RunInScope(context.Background(), handle, "echo hi", []string{"python3", "-c"}, nil, io.Discard, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, []string{"python3", "-c"}, f.lastExecSpec.Shell)
}
