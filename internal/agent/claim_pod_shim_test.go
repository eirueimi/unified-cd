package agent

import (
	"context"
	"io"
	"testing"

	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClaimPod_ToolsDirMountedReadOnlyOnEveryContainer is the regression test
// for Component 3 of the step-shell-shim design ("/.ucd injection"): every
// container the claim pod creates — the pause container included — must
// bind-mount the agent's tools dir read-only at /.ucd, in addition to any
// workspace mount a container already carries.
func TestClaimPod_ToolsDirMountedReadOnlyOnEveryContainer(t *testing.T) {
	f := &podFakeRT{}
	m := newClaimPodManager(f, "/host/w", "/workspace", "pause:img", "runner:img", "/host/tools")
	require.NoError(t, m.Start(context.Background(), mysqlTemplate()))

	require.Len(t, f.created, 3) // pause, mysql, injected "job"

	wantUcdMount := crt.Mount{HostPath: "/host/tools", ContainerPath: "/.ucd", ReadOnly: true}

	pause := f.created[0]
	require.Len(t, pause.Mounts, 1, "pause has no workspace mount, only /.ucd")
	assert.Equal(t, wantUcdMount, pause.Mounts[0])

	for _, spec := range f.created[1:] {
		require.Len(t, spec.Mounts, 2, "workspace mount + /.ucd shim mount")
		assert.Equal(t, "/host/w", spec.Mounts[0].HostPath, "workspace mount stays first")
		assert.Equal(t, "/workspace", spec.Mounts[0].ContainerPath)
		assert.False(t, spec.Mounts[0].ReadOnly, "the workspace mount itself stays read-write")
		assert.Equal(t, wantUcdMount, spec.Mounts[1])
	}
}

// TestClaimPod_Exec_DefaultShellWhenStepShellEmpty verifies claimPodManager.Exec
// applies the shim default (["/.ucd/ucd-sh", "-c"]) when the caller passes a
// nil/empty shell argv — the "step.shell unset" case (api.ClaimStep.Shell nil).
func TestClaimPod_Exec_DefaultShellWhenStepShellEmpty(t *testing.T) {
	f := &podFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "p", "r", "")
	require.NoError(t, m.Start(context.Background(), mysqlTemplate()))

	_, err := m.Exec(context.Background(), "", "echo hi", nil, nil, io.Discard, io.Discard)
	require.NoError(t, err)
	require.NotEmpty(t, f.execSpecs)
	assert.Equal(t, []string{"/.ucd/ucd-sh", "-c"}, f.execSpecs[len(f.execSpecs)-1].Shell)
}

// TestClaimPod_Exec_HonorsExplicitShell verifies claimPodManager.Exec passes
// a non-empty shell argv through verbatim (step.shell: [bash, -lc]).
func TestClaimPod_Exec_HonorsExplicitShell(t *testing.T) {
	f := &podFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "p", "r", "")
	require.NoError(t, m.Start(context.Background(), mysqlTemplate()))

	_, err := m.Exec(context.Background(), "", "echo hi", []string{"bash", "-lc"}, nil, io.Discard, io.Discard)
	require.NoError(t, err)
	require.NotEmpty(t, f.execSpecs)
	assert.Equal(t, []string{"bash", "-lc"}, f.execSpecs[len(f.execSpecs)-1].Shell)
}
