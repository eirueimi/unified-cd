package k8sagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

func TestBuildPod_Fallback(t *testing.T) {
	pod, err := BuildPod("run-abc123", "test-ns", nil, nil, "golang:1.24-alpine", SidecarSpec{})
	require.NoError(t, err)
	require.Len(t, pod.Spec.Containers, 1)
	assert.Equal(t, "job", pod.Spec.Containers[0].Name)
	assert.Equal(t, "golang:1.24-alpine", pod.Spec.Containers[0].Image)
	// workspace emptyDir is automatically injected
	assert.Len(t, pod.Spec.Volumes, 1)
	assert.Equal(t, "workspace", pod.Spec.Volumes[0].Name)
	assert.NotNil(t, pod.Spec.Volumes[0].EmptyDir)
	assert.Equal(t, "/workspace", pod.Spec.Containers[0].VolumeMounts[0].MountPath)
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

	pod, err := BuildPod("run-abc123", "test-ns", agentTmpls, jobTmpl, "fallback:latest", SidecarSpec{})
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

	pod, err := BuildPod("run-abc123", "test-ns", agentTmpls, jobTmpl, "fallback:latest", SidecarSpec{})
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

	pod, err := BuildPod("run-abc123", "test-ns", nil, jobTmpl, "fallback:latest", SidecarSpec{})
	require.NoError(t, err)
	assert.Equal(t, "python:3.12-slim", pod.Spec.Containers[0].Image)
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
	pod, err := BuildPod("run-abc123", "test-ns", agentTmpls, &dsl.PodTemplate{Name: "golang"}, "", SidecarSpec{})
	require.NoError(t, err)
	require.Len(t, pod.Spec.Volumes, 1)
	require.NotNil(t, pod.Spec.Volumes[0].PersistentVolumeClaim)
	assert.Equal(t, "my-pvc", pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
}

func TestBuildPod_ContainersWorkingDirIsWorkspace(t *testing.T) {
	t.Run("defaults to /workspace", func(t *testing.T) {
		pod, err := BuildPod("run-abc123", "test-ns", nil, nil, "golang:1.24-alpine", SidecarSpec{})
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
		pod, err := BuildPod("run-abc123", "test-ns", agentTmpls, &dsl.PodTemplate{Name: "golang"}, "", SidecarSpec{})
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
		pod, err := BuildPod("run-abc123", "test-ns", agentTmpls, &dsl.PodTemplate{Name: "golang"}, "", SidecarSpec{})
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
		SidecarSpec{Image: "sidecar:latest", S3SecretName: "ucd-s3"})
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
		SidecarSpec{Image: "sidecar:latest", S3SecretName: "ucd-s3"})
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

// TestInjectSleepInfinity_OnlyJobContainerKeptAlive is the k8s-side
// regression test for the sidecar-sleep-infinity bug (see
// sidecar-sleep-fix-brief.md): a podTemplate sidecar (e.g. mysql, redis)
// with no explicit command must run its image's own entrypoint — that IS
// the sidecar's service — not "sleep infinity". Only the primary "job"
// container (the exec target for container:-less steps) gets the
// keep-alive when it has no command of its own. Mirrors the host claim
// pod's fix (internal/agent/claim_pod.go's claimPodManager.Start).
func TestInjectSleepInfinity_OnlyJobContainerKeptAlive(t *testing.T) {
	jobTmpl := &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "job", "image": "golang:1.24-alpine"},
			map[string]any{"name": "mysql", "image": "mysql:8"},
		},
	}}

	pod, err := BuildPod("run-abc123", "test-ns", nil, jobTmpl, "fallback:latest", SidecarSpec{})
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

	assert.Equal(t, []string{"sleep", "infinity"}, job.Command,
		"the primary job container must be kept alive as the exec target")
	assert.Empty(t, mysql.Command,
		"a sidecar with no explicit command must run its image's default entrypoint (mysqld), not sleep infinity")
}

// TestInjectSleepInfinity_JobKeepsExplicitCommand confirms a "job" container
// that already sets its own command is left untouched (existing behavior,
// exercised throughout this file's other tests via explicit "sleep 3600").
func TestInjectSleepInfinity_JobKeepsExplicitCommand(t *testing.T) {
	jobTmpl := &dsl.PodTemplate{Spec: map[string]any{
		"containers": []any{
			map[string]any{"name": "job", "image": "golang:1.24-alpine", "command": []any{"go", "version"}},
		},
	}}
	pod, err := BuildPod("run-abc123", "test-ns", nil, jobTmpl, "fallback:latest", SidecarSpec{})
	require.NoError(t, err)
	require.Len(t, pod.Spec.Containers, 1)
	assert.Equal(t, []string{"go", "version"}, pod.Spec.Containers[0].Command)
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
