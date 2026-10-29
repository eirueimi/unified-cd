package agent

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secFakeRT is podFakeRT plus an ordered event log, per-image users for
// crt.ImageInspector, and canned output for exec.
type secFakeRT struct {
	podFakeRT
	events     []string
	imageUsers map[string]string
	execOut    string
}

func (f *secFakeRT) Create(ctx context.Context, s crt.CreateSpec) (crt.ContainerHandle, error) {
	f.events = append(f.events, "create:"+s.Image)
	return f.podFakeRT.Create(ctx, s)
}

func (f *secFakeRT) Exec(ctx context.Context, h crt.ContainerHandle, s crt.ExecSpec, so, se io.Writer) (int, error) {
	f.events = append(f.events, "exec:"+h.ID)
	if f.execOut != "" {
		_, _ = io.WriteString(so, f.execOut)
	}
	return f.podFakeRT.Exec(ctx, h, s, so, se)
}

func (f *secFakeRT) ImageUser(_ context.Context, image string) (string, error) {
	return f.imageUsers[image], nil
}

func secPT(podSC map[string]any, containers ...map[string]any) *dsl.PodTemplate {
	var cs []any
	for _, c := range containers {
		cs = append(cs, c)
	}
	spec := map[string]any{"containers": cs}
	if podSC != nil {
		spec["securityContext"] = podSC
	}
	return &dsl.PodTemplate{Spec: spec}
}

func createdByImage(t *testing.T, f *podFakeRT, image string) crt.CreateSpec {
	t.Helper()
	for _, s := range f.created {
		if s.Image == image {
			return s
		}
	}
	t.Fatalf("no container created for image %q", image)
	return crt.CreateSpec{}
}

func TestClaimPod_PauseIsRootHelperWithJobMounts(t *testing.T) {
	f := &secFakeRT{}
	m := newClaimPodManager(f, "/host/w", "/workspace", "pause:img", "runner:img", "/host/tools")
	m.volumeMounts = []crt.Mount{{HostPath: "/host/v", ContainerPath: "/cache"}}
	require.NoError(t, m.Start(context.Background(), nil))

	pause := f.created[0]
	job := createdByImage(t, &f.podFakeRT, "runner:img")
	assert.Equal(t, "0:0", pause.User)
	assert.Equal(t, job.Mounts, pause.Mounts, "pause carries the same workspace, volume and /.ucd mounts as the job")
	assert.Len(t, pause.Mounts, 3)
}

func TestClaimPod_PodLevelIdentityAppliesToJobAndSidecars(t *testing.T) {
	f := &secFakeRT{}
	pt := secPT(
		map[string]any{"runAsUser": 1000, "fsGroup": 2000, "supplementalGroups": []any{3000}},
		map[string]any{"name": "db", "image": "db:1"},
		map[string]any{"name": "rootcar", "image": "rootcar:1", "securityContext": map[string]any{"runAsUser": 0}},
	)
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m.Start(context.Background(), pt))

	for _, img := range []string{"db:1", "runner:img"} {
		s := createdByImage(t, &f.podFakeRT, img)
		assert.Equal(t, "1000", s.User, img)
		assert.Equal(t, []string{"3000", "2000"}, s.GroupAdd, img)
	}
	root := createdByImage(t, &f.podFakeRT, "rootcar:1")
	assert.Equal(t, "0", root.User, "container-level runAsUser overrides only that sidecar")
	assert.Equal(t, []string{"3000", "2000"}, root.GroupAdd)
}

func TestClaimPod_FsGroupExecBeforeAnyJobCreate(t *testing.T) {
	f := &secFakeRT{execOut: "ucd-sh fsgroup: warning: chmod /workspace/x: denied\nsecond line\n"}
	pt := secPT(map[string]any{"fsGroup": 2000, "fsGroupChangePolicy": "OnRootMismatch"})
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img", "/host/tools")
	m.volumeMounts = []crt.Mount{{HostPath: "/host/v", ContainerPath: "/cache"}}
	var logged strings.Builder
	var batches int
	m.runLog = func(lines []string) { batches++; logged.WriteString(strings.Join(lines, "\n")) }
	require.NoError(t, m.Start(context.Background(), pt))
	assert.Equal(t, 1, batches, "helper output is delivered to the run log in one batch, not one request per line")

	assert.Equal(t, []string{"create:pause:img", "exec:c0", "create:runner:img"}, f.events)
	require.Len(t, f.execSpecs, 1)
	argv := append(append([]string{}, f.execSpecs[0].Shell...), f.execSpecs[0].Script)
	assert.Equal(t, []string{"/.ucd/ucd-sh", "fsgroup", "--policy=OnRootMismatch",
		"--exclude=/workspace", "--exclude=/cache", "2000", "/workspace", "/cache"}, argv)
	assert.Contains(t, logged.String(), "warning: chmod /workspace/x: denied")
}

func TestClaimPod_FsGroupExcludesEveryRootIncludingRelativeVolumes(t *testing.T) {
	// A relative volume mountPath is bind-mounted under the workspace
	// (volumes_mount.go), so the workspace walk would descend into it unless
	// every root is passed as an exclusion.
	f := &secFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img", "")
	m.volumeMounts = []crt.Mount{
		{HostPath: "/host/v1", ContainerPath: "/workspace/data"},
		{HostPath: "/host/v2", ContainerPath: "/cache"},
	}
	require.NoError(t, m.Start(context.Background(), secPT(map[string]any{"fsGroup": 7})))
	require.Len(t, f.execSpecs, 1)
	argv := append(append([]string{}, f.execSpecs[0].Shell...), f.execSpecs[0].Script)
	assert.Equal(t, []string{"/.ucd/ucd-sh", "fsgroup", "--policy=Always",
		"--exclude=/workspace", "--exclude=/workspace/data", "--exclude=/cache",
		"7", "/workspace", "/workspace/data", "/cache"}, argv)
}

func TestClaimPod_FsGroupDefaultsToAlwaysAndSkippedWhenUnset(t *testing.T) {
	f := &secFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m.Start(context.Background(), secPT(map[string]any{"fsGroup": 5})))
	require.Len(t, f.execSpecs, 1)
	assert.Contains(t, f.execSpecs[0].Shell, "--policy=Always")

	f2 := &secFakeRT{}
	m2 := newClaimPodManager(f2, "/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m2.Start(context.Background(), secPT(map[string]any{"runAsUser": 1000})))
	assert.Empty(t, f2.execSpecs, "no fsGroup, no fsgroup exec")
}

func TestClaimPod_RunAsNonRootRejectsRootImage(t *testing.T) {
	f := &secFakeRT{imageUsers: map[string]string{"runner:img": ""}}
	pt := secPT(map[string]any{"runAsNonRoot": true})
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img", "")
	err := m.Start(context.Background(), pt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "container has runAsNonRoot and image will run as root")
	assert.Equal(t, []string{"create:pause:img"}, f.events, "no job or sidecar was created")
}

func TestClaimPod_RunAsNonRootRejectsNamedImageUser(t *testing.T) {
	f := &secFakeRT{imageUsers: map[string]string{"runner:img": "node"}}
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img", "")
	err := m.Start(context.Background(), secPT(map[string]any{"runAsNonRoot": true}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image has non-numeric user (node), cannot verify user is non-root")
	assert.Equal(t, []string{"create:pause:img"}, f.events)
}

func TestClaimPod_RunAsNonRootAcceptsNumericImageUserAndExplicitUID(t *testing.T) {
	f := &secFakeRT{imageUsers: map[string]string{"runner:img": "1001:1001"}}
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m.Start(context.Background(), secPT(map[string]any{"runAsNonRoot": true})))
	assert.Equal(t, "", createdByImage(t, &f.podFakeRT, "runner:img").User, "image default user is kept")

	f2 := &secFakeRT{}
	m2 := newClaimPodManager(f2, "/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m2.Start(context.Background(), secPT(map[string]any{"runAsNonRoot": true, "runAsUser": 1000})))

	f3 := &secFakeRT{}
	m3 := newClaimPodManager(f3, "/w", "/workspace", "pause:img", "runner:img", "")
	err := m3.Start(context.Background(), secPT(map[string]any{"runAsNonRoot": true, "runAsUser": 0}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runAsNonRoot")
	assert.Equal(t, []string{"create:pause:img"}, f3.events)
}

func TestClaimPod_RunAsGroupWithoutUserUsesImageUser(t *testing.T) {
	f := &secFakeRT{imageUsers: map[string]string{"runner:img": "node"}}
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m.Start(context.Background(), secPT(map[string]any{"runAsGroup": 4000})))
	assert.Equal(t, "node:4000", createdByImage(t, &f.podFakeRT, "runner:img").User)

	f2 := &secFakeRT{imageUsers: map[string]string{"runner:img": "1001:55"}}
	m2 := newClaimPodManager(f2, "/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m2.Start(context.Background(), secPT(map[string]any{"runAsGroup": 4000})))
	assert.Equal(t, "1001:4000", createdByImage(t, &f2.podFakeRT, "runner:img").User)

	f3 := &secFakeRT{imageUsers: map[string]string{"runner:img": ""}}
	m3 := newClaimPodManager(f3, "/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m3.Start(context.Background(), secPT(map[string]any{"runAsGroup": 4000})))
	assert.Equal(t, "0:4000", createdByImage(t, &f3.podFakeRT, "runner:img").User)
}

func TestClaimPod_ImageUserNeededButRuntimeCannotInspect(t *testing.T) {
	f := &podFakeRT{} // no ImageInspector
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img", "")
	err := m.Start(context.Background(), secPT(map[string]any{"runAsNonRoot": true}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot inspect")
	assert.Len(t, f.created, 1, "only pause was created")

	// Not needed (explicit uid and gid): works without an inspector.
	f2 := &podFakeRT{}
	m2 := newClaimPodManager(f2, "/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m2.Start(context.Background(), secPT(map[string]any{"runAsNonRoot": true, "runAsUser": 1, "runAsGroup": 2})))
}

func TestClaimPod_NoSecurityContextChangesNothing(t *testing.T) {
	f := &secFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m.Start(context.Background(), mysqlTemplate()))
	for _, s := range f.created[1:] {
		assert.Equal(t, "", s.User)
		assert.Nil(t, s.GroupAdd)
	}
	assert.Empty(t, f.execSpecs)
}

func TestClaimPod_RootlessRangeLimit(t *testing.T) {
	f := &secFakeRT{}
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img", "")
	m.maxMappedID = 65535
	err := m.Start(context.Background(), secPT(map[string]any{"runAsUser": 200000}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runAsUser 200000 is not mapped by this agent's rootless container runtime (mapped container IDs 0-65535)")
	assert.Empty(t, f.events, "fails before any Create")

	for name, sc := range map[string]map[string]any{
		"fsGroup":            {"fsGroup": 70000},
		"supplementalGroups": {"supplementalGroups": []any{70000}},
		"runAsGroup":         {"runAsGroup": 70000},
	} {
		f := &secFakeRT{}
		m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img", "")
		m.maxMappedID = 65535
		err := m.Start(context.Background(), secPT(sc))
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), name)
		assert.Empty(t, f.events, name)
	}

	// 0 means no limit; an in-range ID passes.
	f2 := &secFakeRT{}
	m2 := newClaimPodManager(f2, "/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m2.Start(context.Background(), secPT(map[string]any{"runAsUser": 200000})))
	f3 := &secFakeRT{}
	m3 := newClaimPodManager(f3, "/w", "/workspace", "pause:img", "runner:img", "")
	m3.maxMappedID = 65535
	require.NoError(t, m3.Start(context.Background(), secPT(map[string]any{"runAsUser": 65535})))
}

type captureHandler struct{ msgs *[]string }

func (captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	*h.msgs = append(*h.msgs, r.Level.String()+" "+r.Message)
	return nil
}
func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

func TestParseContainerDef_NoWarnForSecurityContext(t *testing.T) {
	var msgs []string
	old := slog.Default()
	slog.SetDefault(slog.New(captureHandler{&msgs}))
	defer slog.SetDefault(old)

	_, err := parseContainerDef("c", map[string]any{
		"name": "c", "image": "x", "securityContext": map[string]any{"runAsUser": 1},
	})
	require.NoError(t, err)
	assert.Empty(t, msgs)
}

func TestClaimPod_HardeningFlagsPerContainer(t *testing.T) {
	f := &secFakeRT{}
	pt := secPT(nil,
		map[string]any{"name": "job", "image": "runner:img", "securityContext": map[string]any{
			"allowPrivilegeEscalation": false,
			"readOnlyRootFilesystem":   true,
			"capabilities":             map[string]any{"drop": []any{"ALL"}, "add": []any{"NET_BIND_SERVICE"}},
		}},
		map[string]any{"name": "db", "image": "db:1"},
	)
	m := newClaimPodManager(f, "/w", "/workspace", "pause:img", "runner:img", "")
	require.NoError(t, m.Start(context.Background(), pt))

	job := createdByImage(t, &f.podFakeRT, "runner:img")
	assert.Equal(t, []string{"ALL"}, job.CapDrop)
	assert.Equal(t, []string{"NET_BIND_SERVICE"}, job.CapAdd)
	assert.True(t, job.NoNewPrivileges)
	assert.True(t, job.ReadOnlyRootfs)

	db := createdByImage(t, &f.podFakeRT, "db:1")
	assert.Empty(t, db.CapDrop)
	assert.Empty(t, db.CapAdd)
	assert.False(t, db.NoNewPrivileges)
	assert.False(t, db.ReadOnlyRootfs, "hardening is per container, as on Kubernetes")

	pause := f.created[0]
	assert.False(t, pause.ReadOnlyRootfs)
	assert.False(t, pause.NoNewPrivileges)
	assert.Empty(t, pause.CapDrop)
}
