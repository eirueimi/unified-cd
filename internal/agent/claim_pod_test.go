package agent

import (
	"context"
	"io"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// podFakeRT records Create/Exec/Remove calls. Mirror the existing fakeRT in
// scope_test.go but capture CreateSpec per call and return handle IDs "c0",
// "c1", ... in order.
type podFakeRT struct {
	created []crt.CreateSpec
	execs   []struct {
		id     string
		script string
	}
	removed []string
}

func (f *podFakeRT) Name() string                                  { return "fake" }
func (f *podFakeRT) Available() bool                                { return true }
func (f *podFakeRT) Pull(context.Context, string) error             { return nil }
func (f *podFakeRT) Run(context.Context, crt.RunSpec, io.Writer, io.Writer) (int, error) {
	return 0, nil
}
func (f *podFakeRT) Create(_ context.Context, s crt.CreateSpec) (crt.ContainerHandle, error) {
	f.created = append(f.created, s)
	return crt.ContainerHandle{ID: fmtID(len(f.created) - 1)}, nil
}
func (f *podFakeRT) Exec(_ context.Context, h crt.ContainerHandle, s crt.ExecSpec, _, _ io.Writer) (int, error) {
	f.execs = append(f.execs, struct{ id, script string }{h.ID, s.Script})
	return 0, nil
}
func (f *podFakeRT) CopyIn(context.Context, crt.ContainerHandle, string, string) error  { return nil }
func (f *podFakeRT) CopyOut(context.Context, crt.ContainerHandle, string, string) error { return nil }
func (f *podFakeRT) Remove(_ context.Context, h crt.ContainerHandle) error {
	f.removed = append(f.removed, h.ID)
	return nil
}
func fmtID(i int) string { return "c" + string(rune('0'+i)) }

func mysqlTemplate() *dsl.PodTemplate {
	return &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "mysql", "image": "mysql:8"},
		},
	}}
}

func TestClaimPod_StartPauseFirstThenEager(t *testing.T) {
	f := &podFakeRT{}
	m := newClaimPodManager(f, "/host/w", "/workspace", "pause:img", "runner:img")
	require.NoError(t, m.Start(context.Background(), mysqlTemplate()))

	require.Len(t, f.created, 3) // pause, mysql, injected "job"
	pause := f.created[0]
	assert.Equal(t, "pause:img", pause.Image)
	assert.Empty(t, pause.NetworkContainer)
	assert.Empty(t, pause.Mounts, "pause carries no workspace mount")

	for _, spec := range f.created[1:] {
		assert.Equal(t, "c0", spec.NetworkContainer, "every claim container joins the pause netns")
		require.Len(t, spec.Mounts, 1)
		assert.Equal(t, "/host/w", spec.Mounts[0].HostPath)
		assert.Equal(t, "/workspace", spec.Mounts[0].ContainerPath)
		assert.Equal(t, "/workspace", spec.WorkDir)
	}
	assert.Equal(t, "mysql:8", f.created[1].Image)
	assert.Equal(t, "runner:img", f.created[2].Image, "job container injected from runner image")
}

func TestClaimPod_JobFromTemplateNotInjected(t *testing.T) {
	f := &podFakeRT{}
	pt := &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "job", "image": "golang:1.22"},
		},
	}}
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img")
	require.NoError(t, m.Start(context.Background(), pt))
	require.Len(t, f.created, 2) // pause + job (no injection)
	assert.Equal(t, "golang:1.22", f.created[1].Image)
}

func TestClaimPod_NilTemplateGetsDefaultJob(t *testing.T) {
	f := &podFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img")
	require.NoError(t, m.Start(context.Background(), nil))
	require.Len(t, f.created, 2) // pause + injected job
	assert.Equal(t, "runner:img", f.created[1].Image)
}

func TestClaimPod_ExecTargets(t *testing.T) {
	f := &podFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "p", "r")
	require.NoError(t, m.Start(context.Background(), mysqlTemplate()))

	_, err := m.Exec(context.Background(), "", "echo default", nil, io.Discard, io.Discard)
	require.NoError(t, err)
	_, err = m.Exec(context.Background(), "mysql", "echo sidecar", nil, io.Discard, io.Discard)
	require.NoError(t, err)
	_, err = m.Exec(context.Background(), "nope", "x", nil, io.Discard, io.Discard)
	require.Error(t, err, "unknown container name")

	// default targeted the injected job container (created 3rd → id c2),
	// sidecar targeted mysql (id c1)
	assert.Equal(t, "c2", f.execs[0].id)
	assert.Equal(t, "c1", f.execs[1].id)
}

func TestClaimPod_CloseAllRemovesContainersThenPause(t *testing.T) {
	f := &podFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "p", "r")
	require.NoError(t, m.Start(context.Background(), mysqlTemplate()))
	m.CloseAll(context.Background())
	require.Len(t, f.removed, 3)
	assert.Equal(t, "c0", f.removed[len(f.removed)-1], "pause removed last")
}

// TestClaimContainerDefs_InjectsJobWhenAbsent covers claimContainerDefs
// directly: no "job" container in the template → injected primary appended.
func TestClaimContainerDefs_InjectsJobWhenAbsent(t *testing.T) {
	defs := claimContainerDefs(mysqlTemplate(), "runner:img")
	require.Len(t, defs, 2)
	assert.Equal(t, "mysql", defs[0].Name)
	assert.Equal(t, containerDef{Name: "job", Image: "runner:img"}, defs[1])
}

// TestClaimContainerDefs_NilTemplate covers the nil-podTemplate case: just
// the injected primary, the host twin of k8s defaultPodSpec.
func TestClaimContainerDefs_NilTemplate(t *testing.T) {
	defs := claimContainerDefs(nil, "runner:img")
	require.Len(t, defs, 1)
	assert.Equal(t, containerDef{Name: "job", Image: "runner:img"}, defs[0])
}

// TestClaimContainerDefs_JobPresentNotDuplicated covers a template that
// already defines "job": it is used as-is and not duplicated with the
// injected default.
func TestClaimContainerDefs_JobPresentNotDuplicated(t *testing.T) {
	pt := &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "job", "image": "golang:1.22"},
		},
	}}
	defs := claimContainerDefs(pt, "runner:img")
	require.Len(t, defs, 1)
	assert.Equal(t, "golang:1.22", defs[0].Image)
}

// TestParseContainerDef_WarnsOnUnsupportedField documents (via absence of a
// panic/error — WARN is a log side effect) that unsupported podTemplate
// container fields are ignored rather than rejected. This is exercised
// through claimContainerDefs, the only remaining caller path once
// namedContainerManager is retired in Task 7.
func TestParseContainerDef_WarnsOnUnsupportedField(t *testing.T) {
	pt := &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{
				"name":    "tools",
				"image":   "node:20",
				"command": []any{"/bin/sh"}, // unsupported: triggers WARN, not an error
			},
		},
	}}
	defs := claimContainerDefs(pt, "runner:img")
	require.Len(t, defs, 2)
	assert.Equal(t, "tools", defs[0].Name)
	assert.Equal(t, "node:20", defs[0].Image)
}
