package k8sagent

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

func wsTmpl(mp string) *dsl.PodTemplate {
	return &dsl.PodTemplate{Workspace: &dsl.WorkspaceConfig{MountPath: mp}}
}

func TestCheckJobMountPaths_WorkspaceProtectedDirs(t *testing.T) {
	dirs := append(append([]string{}, dsl.ProtectedMountDirs...), sidecarProtectedDirs...)
	for _, d := range dirs {
		cases := []string{d, d + "/work"}
		if i := strings.LastIndex(d, "/"); i > 0 {
			cases = append(cases, d[:i]) // ancestor
		}
		for _, mp := range cases {
			err := CheckJobMountPaths(wsTmpl(mp), mp, nil)
			require.Error(t, err, mp)
			var pe *JobPodPolicyError
			require.ErrorAs(t, err, &pe)
			require.Len(t, pe.Violations, 1)
			assert.Empty(t, pe.Violations[0].Allow, "no setting may allow %s", mp)
			assert.Contains(t, err.Error(), "k8s: job pod policy:")
			assert.Contains(t, err.Error(), "no k8s-agent setting allows this")
			assert.Contains(t, err.Error(), "podTemplate.workspace.mountPath")
		}
	}
	for _, mp := range []string{"/", "workspace", "./workspace", "/workspace/", "/a/../usr"} {
		require.Error(t, CheckJobMountPaths(wsTmpl(mp), mp, nil), mp)
	}
}

func TestCheckJobMountPaths_NormalPathsAllowed(t *testing.T) {
	for _, mp := range []string{"/workspace", "/home/runner/work"} {
		require.NoError(t, CheckJobMountPaths(wsTmpl(mp), mp, nil), mp)
	}
	require.NoError(t, CheckJobMountPaths(nil, "/workspace", nil))
	require.NoError(t, CheckJobMountPaths(&dsl.PodTemplate{}, "/workspace", nil))
	vols := []dsl.VolumeMount{
		{Name: dsl.StringOrSlice{"a"}, MountPath: "cache"},
		{Name: dsl.StringOrSlice{"b"}, MountPath: "."},
		{Name: dsl.StringOrSlice{"c"}, MountPath: "/go/cache"},
	}
	require.NoError(t, CheckJobMountPaths(nil, "/workspace", vols))
}

// An agent template's workspace is trusted; only a job-supplied one is checked.
func TestCheckJobMountPaths_AgentTemplateWorkspaceNotChecked(t *testing.T) {
	// The job names a template and supplies no workspace of its own.
	require.NoError(t, CheckJobMountPaths(&dsl.PodTemplate{Name: "t"}, "/usr/local/bin", nil))
}

func TestCheckJobMountPaths_VolumeMountPaths(t *testing.T) {
	vol := func(mp string) []dsl.VolumeMount {
		return []dsl.VolumeMount{{Name: dsl.StringOrSlice{"v"}, MountPath: mp}}
	}
	for _, mp := range []string{"/usr/local/bin", "/usr/local/bin/x", "/usr", "/etc/unified-cd/s3-credentials", "/.ucd", "/"} {
		err := CheckJobMountPaths(nil, "/workspace", vol(mp))
		require.Error(t, err, mp)
		assert.Contains(t, err.Error(), "spec.volumes[0].mountPath")
	}
	// A relative path that climbs out of the workspace is resolved first.
	require.Error(t, CheckJobMountPaths(nil, "/workspace", vol("../usr/local/bin")))
	require.Error(t, CheckJobMountPaths(nil, "/workspace", vol("../../usr")))
	// A workspace-relative path is covered by the workspace check.
	require.NoError(t, CheckJobMountPaths(nil, "/workspace", vol("deps/cache")))
}

func TestCheckJobMountPaths_ReportsEveryViolation(t *testing.T) {
	vols := []dsl.VolumeMount{{Name: dsl.StringOrSlice{"v"}, MountPath: "/usr/local/bin"}}
	err := CheckJobMountPaths(wsTmpl("/etc"), "/etc", vols)
	var pe *JobPodPolicyError
	require.ErrorAs(t, err, &pe)
	assert.Len(t, pe.Violations, 2)
}

func TestK8sExecuteRun_ProtectedWorkspaceMountPathRefused(t *testing.T) {
	const agentID, runID = "k8s-mp-1", "run-mp-1"
	s := newPolicyRunServer(t, agentID, runID)
	pm := &fakePM{}
	a := &K8sAgent{cfg: Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04"}, client: s.client, pm: pm, exec: &fakeExec{}}
	// Even with every opt-in set, no setting allows this.
	a.cfg.JobPodPolicy.AllowPrivileged = true
	a.cfg.JobPodPolicy.AllowHostAccess = true

	claim := api.ClaimResponse{
		RunID: runID, JobName: "mp",
		PodTemplate: wsTmpl("/usr/local/bin"),
		Stages:      []api.ClaimStage{{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "build", Run: "echo hi"}}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.executeRun(ctx, claim)

	require.Equal(t, api.RunFailed, s.waitFinish(t))
	lines := s.logLines()
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "k8s: job pod policy:")
	require.Contains(t, lines[0], "podTemplate.workspace.mountPath")
	require.Nil(t, pm.created, "a refused run must never create a Pod")
}

func TestK8sExecuteRun_ProtectedVolumeMountPathRefused(t *testing.T) {
	const agentID, runID = "k8s-mp-2", "run-mp-2"
	s := newPolicyRunServer(t, agentID, runID)
	pm := &fakePM{}
	a := &K8sAgent{cfg: Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04"}, client: s.client, pm: pm, exec: &fakeExec{}}

	claim := api.ClaimResponse{
		RunID: runID, JobName: "mp",
		Volumes: []dsl.VolumeMount{{Name: dsl.StringOrSlice{"v"}, MountPath: "/usr/local/bin"}},
		Stages:  []api.ClaimStage{{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "build", Run: "echo hi"}}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.executeRun(ctx, claim)

	require.Equal(t, api.RunFailed, s.waitFinish(t))
	lines := s.logLines()
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "spec.volumes[0].mountPath")
	require.Nil(t, pm.created)
}

// The sidecar is started and exec'd by absolute path, so a job-chosen mount
// can never shadow a PATH entry the exec would resolve through.
func TestSidecarIsAddressedByAbsolutePath(t *testing.T) {
	assert.Equal(t, "/usr/local/bin/unified-sidecar", sidecarBinary)
	sc, _ := buildArtifactSidecarContainer(SidecarSpec{Image: "img"})
	require.Equal(t, []string{sidecarBinary, "idle"}, sc.Command)

	ctx := context.Background()
	for name, call := range map[string]func(b *k8sBackend) error{
		"cache restore": func(b *k8sBackend) error {
			_, err := b.CacheRestore(ctx, agentlib.ScopeHandle{}, "k", nil, "/workspace/c")
			return err
		},
		"cache save": func(b *k8sBackend) error {
			return b.CacheSave(ctx, agentlib.ScopeHandle{}, "k", "/workspace/c", 7)
		},
		"artifact upload": func(b *k8sBackend) error {
			return b.UploadArtifact(ctx, agentlib.ScopeHandle{}, "r", "a", "/workspace/c")
		},
		"artifact download": func(b *k8sBackend) error {
			return b.DownloadArtifact(ctx, agentlib.ScopeHandle{}, "r", "a", "/workspace/c")
		},
	} {
		t.Run(name, func(t *testing.T) {
			ex := &fakeExec{stdout: "UCD_CACHE_RESULT=hit\n"}
			b := newK8sBackend(&K8sAgent{exec: ex}, "run-1", "job", "pod", "/workspace", nil, metav1.Time{})
			_ = call(b)
			assert.True(t, strings.HasPrefix(ex.gotScript, "/usr/local/bin/unified-sidecar "), "argv was %q", ex.gotScript)
		})
	}
}

// The agent turns a backslash into a separator when it mounts a volume, so the
// protected-directory check must see the same path. Each spelling below is
// mounted at a protected directory; none may get past CheckJobMountPaths, which
// is what runs on a job stored before apply-time validation rejected backslashes.
func TestCheckJobMountPaths_BackslashSpellingsAreCheckedAsMounted(t *testing.T) {
	for _, mp := range []string{
		`\usr\local\bin`,
		`/usr\local\bin`,
		`usr\..\..\usr\local\bin`,
		`..\usr\local\bin`,
		`/etc\unified-cd`,
		`/var\run\secrets\unified-cd-store-credentials`,
		`\.ucd`,
		`/usr/local\bin/x`,
	} {
		vols := []dsl.VolumeMount{{Name: dsl.StringOrSlice{"v"}, MountPath: mp}}
		err := CheckJobMountPaths(nil, "/workspace", vols)
		require.Error(t, err, mp)
		assert.Contains(t, err.Error(), "spec.volumes[0].mountPath", mp)
	}
	// A backslash path that stays under the workspace is still the workspace
	// check's business, and a harmless absolute one is allowed.
	require.NoError(t, CheckJobMountPaths(nil, "/workspace", []dsl.VolumeMount{{Name: dsl.StringOrSlice{"v"}, MountPath: `deps\cache`}}))
	require.NoError(t, CheckJobMountPaths(nil, "/workspace", []dsl.VolumeMount{{Name: dsl.StringOrSlice{"v"}, MountPath: `/go\pkg\mod`}}))
}

// A backslash in the job's own workspace.mountPath is refused too.
func TestCheckJobMountPaths_WorkspaceBackslashRefused(t *testing.T) {
	for _, mp := range []string{`\usr\local\bin`, `/usr\local`, `/work\space`} {
		require.Error(t, CheckJobMountPaths(wsTmpl(mp), "/workspace", nil), mp)
	}
}

// What the check looks at is what the mount code mounts: for every spelling,
// the path injectVolumes gives the sidecar equals the one CheckJobMountPaths
// resolved. If one of them is changed without the other this fails.
func TestVolumeMountPathCheckAndMountAgree(t *testing.T) {
	for _, mp := range []string{`\usr\local\bin`, `/usr\local\bin`, `usr\..\..\usr\local\bin`, `a/../b`, `/data//x/`, `deps\cache`} {
		norm := normalizeVolumeMountPath(mp)
		ps := &corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}}
		injectVolumes(ps, "/workspace", []PodVolume{{Name: "v", ClaimName: "pvc", MountPath: norm}})
		if norm == "." {
			continue
		}
		require.Len(t, ps.Containers[0].VolumeMounts, 1, mp)
		assert.Equal(t, volumeContainerPath("/workspace", norm), ps.Containers[0].VolumeMounts[0].MountPath, mp)
	}
}

// A stored job that bypassed apply-time validation with a backslash spelling is
// refused on the agent before any Pod is created.
func TestK8sExecuteRun_BackslashVolumeMountPathRefused(t *testing.T) {
	for i, mp := range []string{`\usr\local\bin`, `/usr\local\bin`, `usr\..\..\usr\local\bin`} {
		agentID, runID := fmt.Sprintf("k8s-mp-bs-%d", i), fmt.Sprintf("run-mp-bs-%d", i)
		s := newPolicyRunServer(t, agentID, runID)
		pm := &fakePM{}
		a := &K8sAgent{cfg: Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04"}, client: s.client, pm: pm, exec: &fakeExec{}}
		claim := api.ClaimResponse{
			RunID: runID, JobName: "mp",
			Volumes: []dsl.VolumeMount{{Name: dsl.StringOrSlice{"v"}, MountPath: mp}},
			Stages:  []api.ClaimStage{{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "build", Run: "echo hi"}}},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		a.executeRun(ctx, claim)
		cancel()
		require.Equal(t, api.RunFailed, s.waitFinish(t), mp)
		require.Contains(t, s.logLines()[0], "spec.volumes[0].mountPath", mp)
		require.Nil(t, pm.created, mp)
	}
}
