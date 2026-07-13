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
	// execSpecs mirrors execs but keeps the full ExecSpec (notably Shell),
	// for tests asserting on shell-argv threading (claim_pod_shim_test.go).
	execSpecs []crt.ExecSpec
	removed   []string
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
	f.execSpecs = append(f.execSpecs, s)
	return 0, nil
}
func (f *podFakeRT) CopyIn(context.Context, crt.ContainerHandle, string, string) error  { return nil }
func (f *podFakeRT) CopyOut(context.Context, crt.ContainerHandle, string, string) error { return nil }
func (f *podFakeRT) Remove(_ context.Context, h crt.ContainerHandle) error {
	f.removed = append(f.removed, h.ID)
	return nil
}
func (f *podFakeRT) Logs(context.Context, crt.ContainerHandle, io.Writer, io.Writer) error {
	return nil
}
func (f *podFakeRT) ExitCode(context.Context, crt.ContainerHandle) (int, error) { return 0, nil }
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
	m := newClaimPodManager(f, "/host/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m.Start(context.Background(), mysqlTemplate()))

	require.Len(t, f.created, 3) // pause, mysql, injected "job"
	pause := f.created[0]
	assert.Equal(t, "pause:img", pause.Image)
	assert.Empty(t, pause.NetworkContainer)
	assert.Empty(t, pause.Mounts, "pause carries no /.ucd mount when toolsDir is empty (this test's newClaimPodManager call)")
	assert.Equal(t, []string{"/.ucd/ucd-sh", "pause"}, pause.Entrypoint,
		"pause container must be kept alive explicitly or the netns it owns collapses")

	for _, spec := range f.created[1:] {
		assert.Equal(t, "c0", spec.NetworkContainer, "every claim container joins the pause netns")
		require.Len(t, spec.Mounts, 1)
		assert.Equal(t, "/host/w", spec.Mounts[0].HostPath)
		assert.Equal(t, "/workspace", spec.Mounts[0].ContainerPath)
		assert.Equal(t, "/workspace", spec.WorkDir)
	}
	assert.Equal(t, "mysql:8", f.created[1].Image)
	assert.Nil(t, f.created[1].Entrypoint,
		"a sidecar with no podTemplate command must run its image's default entrypoint (mysqld), not the pause keep-alive")
	assert.Nil(t, f.created[1].Args,
		"a sidecar with no podTemplate args must run its image's default CMD")
	assert.Equal(t, "runner:img", f.created[2].Image, "job container injected from runner image")
	assert.Equal(t, []string{"/.ucd/ucd-sh", "pause"}, f.created[2].Entrypoint,
		"the primary job container is the exec target and must always be kept alive")
	assert.Nil(t, f.created[2].Args, "keep-alive is Entrypoint-only, no trailing Args")
}

// TestClaimPod_SidecarCommandHonored covers a podTemplate sidecar with an
// explicit command/args: the claim pod must run the sidecar's own
// command/args (via containerDef.Entrypoint/Args), not sleep infinity and
// not the image's default entrypoint.
func TestClaimPod_SidecarCommandHonored(t *testing.T) {
	f := &podFakeRT{}
	pt := &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{
				"name":    "redis",
				"image":   "redis:7",
				"command": []any{"redis-server"},
				"args":    []any{"--port", "6380"},
			},
		},
	}}
	m := newClaimPodManager(f, "/host/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m.Start(context.Background(), pt))

	require.Len(t, f.created, 3) // pause, redis, injected "job"
	assert.Equal(t, "redis:7", f.created[1].Image)
	assert.Equal(t, []string{"redis-server"}, f.created[1].Entrypoint,
		"sidecar's own podTemplate command must be honored, not dropped or replaced")
	assert.Equal(t, []string{"--port", "6380"}, f.created[1].Args,
		"sidecar's own podTemplate args must be honored, not dropped or replaced")
}

// TestClaimPod_PrimaryJobIgnoresTemplateCommand covers a podTemplate that
// defines its own "job" container with an explicit (non-keep-alive) command:
// the primary container is always forced to the ucd-sh pause keep-alive
// regardless, since it is the exec target for every container:-less step.
func TestClaimPod_PrimaryJobIgnoresTemplateCommand(t *testing.T) {
	f := &podFakeRT{}
	pt := &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "job", "image": "golang:1.22", "command": []any{"go", "version"}},
		},
	}}
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m.Start(context.Background(), pt))

	require.Len(t, f.created, 2) // pause + job (no injection)
	assert.Equal(t, "golang:1.22", f.created[1].Image)
	assert.Equal(t, []string{"/.ucd/ucd-sh", "pause"}, f.created[1].Entrypoint,
		"the primary job container must always keep-alive, even if the podTemplate set its own command")
	assert.Nil(t, f.created[1].Args,
		"the primary job container's keep-alive clears any podTemplate args too")
}

func TestClaimPod_JobFromTemplateNotInjected(t *testing.T) {
	f := &podFakeRT{}
	pt := &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "job", "image": "golang:1.22"},
		},
	}}
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m.Start(context.Background(), pt))
	require.Len(t, f.created, 2) // pause + job (no injection)
	assert.Equal(t, "golang:1.22", f.created[1].Image)
}

func TestClaimPod_NilTemplateGetsDefaultJob(t *testing.T) {
	f := &podFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m.Start(context.Background(), nil))
	require.Len(t, f.created, 2) // pause + injected job
	assert.Equal(t, "runner:img", f.created[1].Image)
}

func TestClaimPod_ExecTargets(t *testing.T) {
	f := &podFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "p", "r", "")
	require.NoError(t, m.Start(context.Background(), mysqlTemplate()))

	_, err := m.Exec(context.Background(), "", "echo default", nil, nil, io.Discard, io.Discard)
	require.NoError(t, err)
	_, err = m.Exec(context.Background(), "mysql", "echo sidecar", nil, nil, io.Discard, io.Discard)
	require.NoError(t, err)
	_, err = m.Exec(context.Background(), "nope", "x", nil, nil, io.Discard, io.Discard)
	require.Error(t, err, "unknown container name")

	// default targeted the injected job container (created 3rd → id c2),
	// sidecar targeted mysql (id c1)
	assert.Equal(t, "c2", f.execs[0].id)
	assert.Equal(t, "c1", f.execs[1].id)
}

func TestClaimPod_CloseAllRemovesContainersThenPause(t *testing.T) {
	f := &podFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "p", "r", "")
	require.NoError(t, m.Start(context.Background(), mysqlTemplate()))
	m.CloseAll(context.Background())
	require.Len(t, f.removed, 3)
	assert.Equal(t, "c0", f.removed[len(f.removed)-1], "pause removed last")
}

// TestClaimContainerDefs_InjectsJobWhenAbsent covers claimContainerDefs
// directly: no "job" container in the template → injected primary appended.
func TestClaimContainerDefs_InjectsJobWhenAbsent(t *testing.T) {
	defs, err := claimContainerDefs(mysqlTemplate(), "runner:img")
	require.NoError(t, err)
	require.Len(t, defs, 2)
	assert.Equal(t, "mysql", defs[0].Name)
	assert.Equal(t, containerDef{Name: "job", Image: "runner:img"}, defs[1])
}

// TestClaimContainerDefs_NilTemplate covers the nil-podTemplate case: just
// the injected primary, the host twin of k8s defaultPodSpec.
func TestClaimContainerDefs_NilTemplate(t *testing.T) {
	defs, err := claimContainerDefs(nil, "runner:img")
	require.NoError(t, err)
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
	defs, err := claimContainerDefs(pt, "runner:img")
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "golang:1.22", defs[0].Image)
}

// TestClaimContainerDefs_DuplicateNameKeepsFirst covers a malformed
// podTemplate that defines two containers with the same name: the first
// definition wins (deterministic) and the later duplicate is dropped with a
// WARN, rather than silently overwriting the earlier container's handle.
func TestClaimContainerDefs_DuplicateNameKeepsFirst(t *testing.T) {
	pt := &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "tools", "image": "node:18"},
			map[string]any{"name": "tools", "image": "node:20"},
		},
	}}
	defs, err := claimContainerDefs(pt, "runner:img")
	require.NoError(t, err)
	// tools (first def kept) + injected "job" primary.
	require.Len(t, defs, 2)
	assert.Equal(t, "tools", defs[0].Name)
	assert.Equal(t, "node:18", defs[0].Image, "first definition for a duplicate name must win")
	assert.Equal(t, containerDef{Name: "job", Image: "runner:img"}, defs[1])
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
				"name":         "tools",
				"image":        "node:20",
				"volumeMounts": []any{map[string]any{"name": "x", "mountPath": "/x"}}, // unsupported: triggers WARN, not an error
			},
		},
	}}
	defs, err := claimContainerDefs(pt, "runner:img")
	require.NoError(t, err)
	require.Len(t, defs, 2)
	assert.Equal(t, "tools", defs[0].Name)
	assert.Equal(t, "node:20", defs[0].Image)
}

// TestParseContainerDef_CommandAndArgsCarried is the regression test for the
// sidecar-sleep-infinity fix's parseContainerDef change: command/args are no
// longer host-unsupported/WARN-dropped fields — they are parsed into
// containerDef.Entrypoint (command) and containerDef.Args (args) so a
// sidecar's own entrypoint override can be honored by claimPodManager.Start.
func TestParseContainerDef_CommandAndArgsCarried(t *testing.T) {
	def, err := parseContainerDef("redis", map[string]any{
		"name":    "redis",
		"image":   "redis:7",
		"command": []any{"redis-server"},
		"args":    []any{"--port", "6380"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"redis-server"}, def.Entrypoint)
	assert.Equal(t, []string{"--port", "6380"}, def.Args)
}

// TestParseContainerDef_CommandOnlyNoArgs covers the command-with-no-args
// shape (the common sidecar override case): Entrypoint is set, Args is nil.
func TestParseContainerDef_CommandOnlyNoArgs(t *testing.T) {
	def, err := parseContainerDef("tools", map[string]any{
		"name":    "tools",
		"image":   "node:20",
		"command": []any{"/bin/sh", "-c", "node server.js"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"/bin/sh", "-c", "node server.js"}, def.Entrypoint)
	assert.Nil(t, def.Args)
}

// TestParseContainerDef_NoCommandIsNil covers the default (most common
// sidecar) case: no command/args set at all means both Entrypoint and Args
// are nil, so CreateSpec.Entrypoint/Args stay nil and the image's default
// entrypoint+CMD run unmodified.
func TestParseContainerDef_NoCommandIsNil(t *testing.T) {
	def, err := parseContainerDef("mysql", map[string]any{"name": "mysql", "image": "mysql:8"})
	require.NoError(t, err)
	assert.Nil(t, def.Entrypoint)
	assert.Nil(t, def.Args)
}

// TestParseContainerDef_CommandArgsSplit covers a podTemplate container that
// sets both command and args: they land in separate fields (ENTRYPOINT
// override vs CMD override), not merged into one slice.
func TestParseContainerDef_CommandArgsSplit(t *testing.T) {
	def, err := parseContainerDef("web", map[string]any{
		"image":   "nginx",
		"command": []any{"nginx"},
		"args":    []any{"-g", "daemon off;"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"nginx"}, def.Entrypoint)
	assert.Equal(t, []string{"-g", "daemon off;"}, def.Args)
}

// TestParseContainerDef_ArgsOnly covers args-only: Entrypoint stays nil (the
// image's own ENTRYPOINT is used), Args carries the CMD override.
func TestParseContainerDef_ArgsOnly(t *testing.T) {
	def, err := parseContainerDef("web", map[string]any{"image": "nginx", "args": []any{"-t"}})
	require.NoError(t, err)
	assert.Nil(t, def.Entrypoint)
	assert.Equal(t, []string{"-t"}, def.Args)
}

// TestParseContainerDef_Neither_BothNil covers the no-command-no-args shape:
// both fields nil, image default entrypoint+CMD run unmodified.
func TestParseContainerDef_Neither_BothNil(t *testing.T) {
	def, err := parseContainerDef("db", map[string]any{"image": "mysql:8"})
	require.NoError(t, err)
	assert.Nil(t, def.Entrypoint)
	assert.Nil(t, def.Args)
}

// TestParseContainerDef_ResourcesRequestsWarnsIgnored covers parity fix #2:
// resources.requests has no docker/podman equivalent, so it is WARN-logged
// and ignored (not an error) while resources.limits is still honored.
func TestParseContainerDef_ResourcesRequestsWarnsIgnored(t *testing.T) {
	// requests present → parsed OK (no error), limits still applied, requests ignored.
	def, err := parseContainerDef("web", map[string]any{
		"image": "nginx",
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "500m", "memory": "256Mi"},
			"limits":   map[string]any{"cpu": "1", "memory": "512Mi"},
		},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, def.CPULimit) // limits honored
	assert.NotEmpty(t, def.MemLimit)
}

// TestParseContainerDef_NonStringEnvValueErrors covers parity fix #3: a
// non-string env value (e.g. an unquoted number) hard-errors on the host,
// matching k8s' json.Unmarshal-into-string behavior, instead of being
// silently dropped.
func TestParseContainerDef_NonStringEnvValueErrors(t *testing.T) {
	_, err := parseContainerDef("web", map[string]any{
		"image": "nginx",
		"env":   []any{map[string]any{"name": "PORT", "value": 8080}}, // number, not string
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "value must be a string")
}

// TestParseContainerDef_MissingEnvValueStillSkipsNoError covers the
// valueFrom-style case (no "value" key at all): still WARN+skip, NOT an
// error — distinct from the non-string-value hard error above.
func TestParseContainerDef_MissingEnvValueStillSkipsNoError(t *testing.T) {
	// No `value` key at all (valueFrom-style) → still WARN+skip, NOT an error.
	def, err := parseContainerDef("web", map[string]any{
		"image": "nginx",
		"env":   []any{map[string]any{"name": "SECRET"}},
	})
	require.NoError(t, err)
	assert.Empty(t, def.Env)
}

// TestClaimContainerDefs_UnnamedContainerErrors covers parity fix #5: an
// unnamed podTemplate container hard-errors at pod-build time on the host,
// matching the k8s side (BuildPod), instead of being silently skipped.
func TestClaimContainerDefs_UnnamedContainerErrors(t *testing.T) {
	pt := &dsl.PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"image": "nginx"}, // no name
	}}}
	_, err := claimContainerDefs(pt, "runner:latest")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no name")
}
