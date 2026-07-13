package k8sagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

// testShimImage is the shimImage argument used throughout this file's
// BuildPod calls; only the tests specifically about the ucd-shim init
// container assert on its exact value.
const testShimImage = "ghcr.io/eirueimi/unified-cd-k8s-agent:test"

func TestBuildPod_Fallback(t *testing.T) {
	pod, err := BuildPod("run-abc123", "test-ns", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
	require.NoError(t, err)
	require.Len(t, pod.Spec.Containers, 1)
	assert.Equal(t, "job", pod.Spec.Containers[0].Name)
	assert.Equal(t, "golang:1.24-alpine", pod.Spec.Containers[0].Image)
	// workspace emptyDir and the ucd-tools emptyDir are both automatically injected
	assert.Len(t, pod.Spec.Volumes, 2)
	assert.Equal(t, "workspace", pod.Spec.Volumes[0].Name)
	assert.NotNil(t, pod.Spec.Volumes[0].EmptyDir)
	assert.Equal(t, "/workspace", pod.Spec.Containers[0].VolumeMounts[0].MountPath)
	// FIX 2 regression: the bare "podImage, no podTemplate" fallback path
	// must still land on the ucd-sh pause keep-alive via injectKeepAlive —
	// defaultPodSpec used to bake in a literal "sleep infinity" Command,
	// which made injectKeepAlive's skip-when-Command-set guard fire and this
	// path never got ucd-sh. This assertion was previously missing entirely,
	// which is how the bug hid.
	assert.Equal(t, []string{"/.ucd/ucd-sh", "pause"}, pod.Spec.Containers[0].Command)
}

func TestBuildPod_TemplateRef(t *testing.T) {
	agentTmpls := map[string]AgentPodTemplate{
		"golang": {
			Spec: map[string]any{
				"containers": []any{
					map[string]any{
						"name":    "job",
						"image":   "golang:1.24-alpine",
						"command": []any{"sleep", "3600"},
					},
				},
			},
		},
	}
	jobTmpl := &dsl.PodTemplate{Name: "golang"}

	pod, err := BuildPod("run-abc123", "test-ns", agentTmpls, jobTmpl, "fallback:latest", SidecarSpec{}, testShimImage)
	require.NoError(t, err)
	assert.Equal(t, "golang:1.24-alpine", pod.Spec.Containers[0].Image)
}

func TestBuildPod_Override(t *testing.T) {
	agentTmpls := map[string]AgentPodTemplate{
		"golang": {
			Spec: map[string]any{
				"containers": []any{
					map[string]any{"name": "job", "image": "golang:1.24-alpine", "command": []any{"sleep", "3600"}},
				},
			},
		},
	}
	jobTmpl := &dsl.PodTemplate{
		Name: "golang",
		Override: &dsl.PodSpecPatch{
			Containers: []map[string]any{
				{"name": "node", "image": "node:20-alpine", "command": []any{"sleep", "3600"}},
			},
		},
	}

	pod, err := BuildPod("run-abc123", "test-ns", agentTmpls, jobTmpl, "fallback:latest", SidecarSpec{}, testShimImage)
	require.NoError(t, err)
	assert.Len(t, pod.Spec.Containers, 2)
	names := []string{pod.Spec.Containers[0].Name, pod.Spec.Containers[1].Name}
	assert.Contains(t, names, "job")
	assert.Contains(t, names, "node")
}

func TestBuildPod_InlineSpec(t *testing.T) {
	jobTmpl := &dsl.PodTemplate{
		Spec: map[string]any{
			"containers": []any{
				map[string]any{"name": "job", "image": "python:3.12-slim", "command": []any{"sleep", "3600"}},
			},
		},
	}

	pod, err := BuildPod("run-abc123", "test-ns", nil, jobTmpl, "fallback:latest", SidecarSpec{}, testShimImage)
	require.NoError(t, err)
	assert.Equal(t, "python:3.12-slim", pod.Spec.Containers[0].Image)
}

// TestBuildPod_UnnamedContainerErrors covers host/k8s parity fix #5: an
// unnamed podTemplate container hard-errors at pod-build time, matching the
// host claimContainerDefs check, instead of being rejected only later by the
// k8s API server as an opaque run-creation failure.
func TestBuildPod_UnnamedContainerErrors(t *testing.T) {
	jobTmpl := &dsl.PodTemplate{
		Spec: map[string]any{
			"containers": []any{
				map[string]any{"image": "nginx"}, // no name
			},
		},
	}

	_, err := BuildPod("run-abc123", "test-ns", nil, jobTmpl, "fallback:latest", SidecarSpec{}, testShimImage)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no name")
}

func TestBuildPod_WorkspacePVC(t *testing.T) {
	agentTmpls := map[string]AgentPodTemplate{
		"golang": {
			Workspace: &dsl.WorkspaceConfig{PVC: &dsl.WorkspacePVC{ClaimName: "my-pvc"}},
			Spec: map[string]any{
				"containers": []any{
					map[string]any{"name": "job", "image": "golang:1.24-alpine", "command": []any{"sleep", "3600"}},
				},
			},
		},
	}
	pod, err := BuildPod("run-abc123", "test-ns", agentTmpls, &dsl.PodTemplate{Name: "golang"}, "", SidecarSpec{}, testShimImage)
	require.NoError(t, err)
	require.Len(t, pod.Spec.Volumes, 2)
	require.NotNil(t, pod.Spec.Volumes[0].PersistentVolumeClaim)
	assert.Equal(t, "my-pvc", pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
}

func TestBuildPod_ContainersWorkingDirIsWorkspace(t *testing.T) {
	t.Run("defaults to /workspace", func(t *testing.T) {
		pod, err := BuildPod("run-abc123", "test-ns", nil, nil, "golang:1.24-alpine", SidecarSpec{}, testShimImage)
		require.NoError(t, err)
		require.Len(t, pod.Spec.Containers, 1)
		assert.Equal(t, "/workspace", pod.Spec.Containers[0].WorkingDir)
	})

	t.Run("matches customized MountPath", func(t *testing.T) {
		agentTmpls := map[string]AgentPodTemplate{
			"golang": {
				Workspace: &dsl.WorkspaceConfig{MountPath: "/custom-ws"},
				Spec: map[string]any{
					"containers": []any{
						map[string]any{"name": "job", "image": "golang:1.24-alpine", "command": []any{"sleep", "3600"}},
					},
				},
			},
		}
		pod, err := BuildPod("run-abc123", "test-ns", agentTmpls, &dsl.PodTemplate{Name: "golang"}, "", SidecarSpec{}, testShimImage)
		require.NoError(t, err)
		require.Len(t, pod.Spec.Containers, 1)
		assert.Equal(t, "/custom-ws", pod.Spec.Containers[0].WorkingDir)
		assert.Equal(t, "/custom-ws", pod.Spec.Containers[0].VolumeMounts[0].MountPath)
	})

	t.Run("preserves user-set WorkingDir", func(t *testing.T) {
		agentTmpls := map[string]AgentPodTemplate{
			"golang": {
				Spec: map[string]any{
					"containers": []any{
						map[string]any{
							"name":       "job",
							"image":      "golang:1.24-alpine",
							"command":    []any{"sleep", "3600"},
							"workingDir": "/app",
						},
					},
				},
			},
		}
		pod, err := BuildPod("run-abc123", "test-ns", agentTmpls, &dsl.PodTemplate{Name: "golang"}, "", SidecarSpec{}, testShimImage)
		require.NoError(t, err)
		require.Len(t, pod.Spec.Containers, 1)
		assert.Equal(t, "/app", pod.Spec.Containers[0].WorkingDir, "user-set WorkingDir must not be overwritten")
	})
}

func TestInjectWorkspace_AllContainers(t *testing.T) {
	spec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "job", Image: "golang:1.24-alpine"},
			{Name: "node", Image: "node:20-alpine"},
		},
	}
	injectWorkspace(spec, nil)
	for _, c := range spec.Containers {
		require.Len(t, c.VolumeMounts, 1, "container %s should have workspace mount", c.Name)
		assert.Equal(t, "/workspace", c.VolumeMounts[0].MountPath)
	}
}

func TestBuildPod_InjectsArtifactSidecar(t *testing.T) {
	pod, err := BuildPod("run1", "ns", nil, nil, "job-image:latest",
		SidecarSpec{Image: "sidecar:latest", S3SecretName: "ucd-s3"}, testShimImage)
	require.NoError(t, err)

	var sidecar *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == artifactSidecarName {
			sidecar = &pod.Spec.Containers[i]
		}
	}
	require.NotNil(t, sidecar, "pod must include the unified-artifact sidecar")
	assert.Equal(t, "sidecar:latest", sidecar.Image)

	// Sidecar shares the workspace mount.
	var hasWorkspace bool
	for _, m := range sidecar.VolumeMounts {
		if m.Name == "workspace" {
			hasWorkspace = true
		}
	}
	assert.True(t, hasWorkspace, "sidecar must mount the workspace volume")

	// Sidecar gets its S3 credentials via EnvFrom the Secret (direct-S3 model);
	// no controller URL/token env is injected.
	require.Len(t, sidecar.EnvFrom, 1)
	require.NotNil(t, sidecar.EnvFrom[0].SecretRef)
	assert.Equal(t, "ucd-s3", sidecar.EnvFrom[0].SecretRef.Name)
}

func TestBuildPod_SidecarSecretEnvAndIdle(t *testing.T) {
	pod, err := BuildPod("run1", "ns", nil, nil, "job-image:latest",
		SidecarSpec{Image: "sidecar:latest", S3SecretName: "ucd-s3"}, testShimImage)
	if err != nil {
		t.Fatal(err)
	}
	var sc *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == artifactSidecarName {
			sc = &pod.Spec.Containers[i]
		}
	}
	if sc == nil {
		t.Fatal("sidecar container not found")
	}
	if len(sc.EnvFrom) != 1 || sc.EnvFrom[0].SecretRef == nil || sc.EnvFrom[0].SecretRef.Name != "ucd-s3" {
		t.Fatalf("expected EnvFrom secretRef ucd-s3, got %+v", sc.EnvFrom)
	}
	if len(sc.Command) < 2 || sc.Command[0] != "unified-sidecar" || sc.Command[1] != "idle" {
		t.Fatalf("expected command [unified-sidecar idle], got %v", sc.Command)
	}
}

// TestInjectKeepAlive_OnlyJobContainerKeptAlive is the k8s-side regression
// test for the sidecar-sleep-infinity bug (see sidecar-sleep-fix-brief.md): a
// podTemplate sidecar (e.g. mysql, redis) with no explicit command must run
// its image's own entrypoint — that IS the sidecar's service — not the
// keep-alive. Only the primary "job" container (the exec target for
// container:-less steps) gets the keep-alive when it has no command of its
// own. Mirrors the host claim pod's fix (internal/agent/claim_pod.go's
// claimPodManager.Start). The keep-alive itself is now the injected shim's
// pause subcommand (Component 4 of the step-shell-shim design spec), not
// "sleep infinity".
func TestInjectKeepAlive_OnlyJobContainerKeptAlive(t *testing.T) {
	jobTmpl := &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "job", "image": "golang:1.24-alpine"},
			map[string]any{"name": "mysql", "image": "mysql:8"},
		},
	}}

	pod, err := BuildPod("run-abc123", "test-ns", nil, jobTmpl, "fallback:latest", SidecarSpec{}, testShimImage)
	require.NoError(t, err)

	var job, mysql *corev1.Container
	for i := range pod.Spec.Containers {
		switch pod.Spec.Containers[i].Name {
		case "job":
			job = &pod.Spec.Containers[i]
		case "mysql":
			mysql = &pod.Spec.Containers[i]
		}
	}
	require.NotNil(t, job, "job container must be present")
	require.NotNil(t, mysql, "mysql sidecar must be present")

	assert.Equal(t, []string{"/.ucd/ucd-sh", "pause"}, job.Command,
		"the primary job container must be kept alive as the exec target, via the ucd-sh shim's pause subcommand")
	assert.Empty(t, mysql.Command,
		"a sidecar with no explicit command must run its image's default entrypoint (mysqld), not the keep-alive")
}

func TestInjectKeepAlive_JobForcesPauseOverExplicitCommand(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{
		{Name: "job", Image: "img", Command: []string{"my-server", "--port", "80"}},
	}}
	injectKeepAlive(spec)
	// The primary job's own command is discarded — it must keep-alive.
	assert.Equal(t, []string{ucdMountPath + "/ucd-sh", "pause"}, spec.Containers[0].Command)
	assert.Nil(t, spec.Containers[0].Args)
}

func TestInjectKeepAlive_JobForcesPauseOverExplicitArgs(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{
		{Name: "job", Image: "img", Args: []string{"--flag"}},
	}}
	injectKeepAlive(spec)
	assert.Equal(t, []string{ucdMountPath + "/ucd-sh", "pause"}, spec.Containers[0].Command)
	assert.Nil(t, spec.Containers[0].Args)
}

// TestBuildPod_UcdShimInitContainer asserts the ucd-shim init container is
// prepended with the exact image/command the spec requires, and that it
// mounts the ucd-tools volume at /.ucd.
func TestBuildPod_UcdShimInitContainer(t *testing.T) {
	pod, err := BuildPod("run-abc123", "test-ns", nil, nil, "golang:1.24-alpine", SidecarSpec{}, "ghcr.io/eirueimi/unified-cd-k8s-agent:latest")
	require.NoError(t, err)

	require.Len(t, pod.Spec.InitContainers, 1)
	initC := pod.Spec.InitContainers[0]
	assert.Equal(t, "ucd-shim", initC.Name)
	assert.Equal(t, "ghcr.io/eirueimi/unified-cd-k8s-agent:latest", initC.Image)
	assert.Equal(t, []string{"/ucd-sh", "--install", "/.ucd/ucd-sh"}, initC.Command)

	require.Len(t, initC.VolumeMounts, 1)
	assert.Equal(t, "ucd-tools", initC.VolumeMounts[0].Name)
	assert.Equal(t, "/.ucd", initC.VolumeMounts[0].MountPath)

	var ucdVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "ucd-tools" {
			ucdVol = &pod.Spec.Volumes[i]
		}
	}
	require.NotNil(t, ucdVol, "pod must declare the ucd-tools emptyDir volume")
	assert.NotNil(t, ucdVol.EmptyDir)
}

// TestBuildPod_UcdShimMountOnEveryContainer asserts /.ucd is mounted on the
// primary "job" container, every podTemplate sidecar, AND the injected
// artifact sidecar — a sidecar is itself a container: exec target, so it
// needs the shim just like the primary (Component 3 of the step-shell-shim
// design spec).
func TestBuildPod_UcdShimMountOnEveryContainer(t *testing.T) {
	jobTmpl := &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "job", "image": "golang:1.24-alpine"},
			map[string]any{"name": "mysql", "image": "mysql:8"},
			map[string]any{"name": "redis", "image": "redis:7"},
		},
	}}
	pod, err := BuildPod("run-abc123", "test-ns", nil, jobTmpl, "fallback:latest",
		SidecarSpec{Image: "sidecar:latest"}, testShimImage)
	require.NoError(t, err)

	require.Len(t, pod.Spec.Containers, 4, "job, mysql, redis, and the artifact sidecar")
	for _, c := range pod.Spec.Containers {
		var hasUcdMount bool
		for _, m := range c.VolumeMounts {
			if m.Name == "ucd-tools" && m.MountPath == "/.ucd" {
				hasUcdMount = true
			}
		}
		assert.True(t, hasUcdMount, "container %q must mount /.ucd", c.Name)
	}
}

// TestBuildPod_UcdShimInitContainerIsFirst is the init-container ORDERING
// regression test: a podTemplate that already declares its own
// initContainers must still end up with ucd-shim FIRST, since Kubernetes
// runs InitContainers strictly in order and every later init container (or
// regular container) may itself want to rely on /.ucd being installed.
func TestBuildPod_UcdShimInitContainerIsFirst(t *testing.T) {
	jobTmpl := &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "job", "image": "golang:1.24-alpine"},
		},
		"initContainers": []any{
			map[string]any{"name": "warmup", "image": "busybox:1.36", "command": []any{"echo", "warm"}},
		},
	}}
	pod, err := BuildPod("run-abc123", "test-ns", nil, jobTmpl, "fallback:latest", SidecarSpec{}, testShimImage)
	require.NoError(t, err)

	require.Len(t, pod.Spec.InitContainers, 2, "ucd-shim plus the podTemplate's own initContainer")
	assert.Equal(t, "ucd-shim", pod.Spec.InitContainers[0].Name,
		"ucd-shim must run before any podTemplate-declared init container")
	assert.Equal(t, "warmup", pod.Spec.InitContainers[1].Name)
}

func TestMergeContainers(t *testing.T) {
	base := []corev1.Container{{Name: "job", Image: "golang:1.24-alpine"}}
	patch := []corev1.Container{
		{Name: "job", Image: "golang:1.23-alpine"}, // overwrite
		{Name: "node", Image: "node:20-alpine"},    // add
	}
	result := mergeContainers(base, patch)
	assert.Len(t, result, 2)
	assert.Equal(t, "golang:1.23-alpine", result[0].Image)
	assert.Equal(t, "node", result[1].Name)
}
