package k8sagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestBuildScopePodHasScratchAndSidecarNoWorkspacePVC(t *testing.T) {
	pod := buildScopePod("run123", "ci", "scope:build", "golang:1.22",
		map[string]string{"K": "V"}, SidecarSpec{Image: "sidecar:1", S3SecretName: "s3"}, testShimImage)

	// scratch volume is emptyDir, mounted by both the step and the sidecar
	var scratch *string
	for _, v := range pod.Spec.Volumes {
		if v.Name == "workspace" {
			if v.EmptyDir == nil {
				t.Fatal("scope scratch volume must be emptyDir, not a PVC")
			}
			n := v.Name
			scratch = &n
		}
	}
	if scratch == nil {
		t.Fatal("missing scratch volume")
	}
	names := map[string]bool{}
	for _, c := range pod.Spec.Containers {
		names[c.Name] = true
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name == "workspace" {
				mounted = true
			}
		}
		if !mounted {
			t.Fatalf("container %q does not mount the scratch volume", c.Name)
		}
	}
	if !names["step"] {
		t.Fatal("missing step container")
	}
}

// TestBuildScopePod_UcdShimInitContainer mirrors
// TestBuildPod_UcdShimInitContainer for the scope pod: a uses-scope pod must
// get the exact same ucd-shim init container as the run/pooled pod (Component
// 3 of the step-shell-shim design spec) — this is the FIX 1 regression test
// for the gap where uses-scope pods had no /.ucd at all.
func TestBuildScopePod_UcdShimInitContainer(t *testing.T) {
	pod := buildScopePod("run123", "ci", "scope:build", "golang:1.22",
		nil, SidecarSpec{}, "ghcr.io/eirueimi/unified-cd-k8s-agent:latest")

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
	require.NotNil(t, ucdVol, "scope pod must declare the ucd-tools emptyDir volume")
	assert.NotNil(t, ucdVol.EmptyDir)
}

// TestBuildScopePod_UcdShimMountOnEveryContainer mirrors
// TestBuildPod_UcdShimMountOnEveryContainer: /.ucd must be mounted on BOTH
// the "step" container and the scope's own artifact sidecar, since either
// can be an exec target.
func TestBuildScopePod_UcdShimMountOnEveryContainer(t *testing.T) {
	pod := buildScopePod("run123", "ci", "scope:build", "golang:1.22",
		nil, SidecarSpec{Image: "sidecar:latest"}, testShimImage)

	require.Len(t, pod.Spec.Containers, 2, "step and the artifact sidecar")
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

// TestBuildScopePod_KeepAliveArgv confirms the "step" container's keep-alive
// command is the shared ucdKeepAliveArgv() (the ucd-sh pause subcommand),
// not a hardcoded "sleep infinity" literal — the scope pod's exec target
// must behave identically to the run/pooled pod's "job" container.
func TestBuildScopePod_KeepAliveArgv(t *testing.T) {
	pod := buildScopePod("run123", "ci", "scope:build", "golang:1.22",
		nil, SidecarSpec{}, testShimImage)

	require.Len(t, pod.Spec.Containers, 1)
	assert.Equal(t, ucdKeepAliveArgv(), pod.Spec.Containers[0].Command)
}
