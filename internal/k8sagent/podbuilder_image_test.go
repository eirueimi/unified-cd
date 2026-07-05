package k8sagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestBuildImageStepPod(t *testing.T) {
	pod := buildImageStepPod("run-abcdef0123456789xyz", "ci", "alpine:3.20",
		map[string]string{"FOO": "bar", "UNIFIED_AGENT_OS": "linux"}, 1800)

	// naming + labels (GenerateName suffix = first 16 chars of runID: "run-abcdef012345")
	assert.Equal(t, "ucd-img-run-abcdef012345-", pod.GenerateName)
	assert.Empty(t, pod.Name, "must use GenerateName, not a fixed Name")
	assert.Equal(t, "ci", pod.Namespace)
	assert.Equal(t, "unified-cd-agent", pod.Labels["app"])
	assert.Equal(t, "run-abcdef0123456789xyz", pod.Labels["unified-cd/runId"])

	// single container, sleep infinity, image
	require.Len(t, pod.Spec.Containers, 1)
	c := pod.Spec.Containers[0]
	assert.Equal(t, "step", c.Name)
	assert.Equal(t, "alpine:3.20", c.Image)
	assert.Equal(t, []string{"sleep", "infinity"}, c.Command)

	// env present (sorted, deterministic)
	require.Len(t, c.Env, 2)
	assert.Equal(t, "FOO", c.Env[0].Name)
	assert.Equal(t, "bar", c.Env[0].Value)
	assert.Equal(t, "UNIFIED_AGENT_OS", c.Env[1].Name)
	assert.Equal(t, "linux", c.Env[1].Value)

	// isolation: no workspace volume, no sidecar container
	assert.Empty(t, pod.Spec.Volumes, "image pod must not mount a workspace volume")
	for _, cc := range pod.Spec.Containers {
		assert.NotEqual(t, artifactSidecarName, cc.Name, "image pod must not inject the artifact sidecar")
	}

	// lifecycle guards
	assert.Equal(t, corev1.RestartPolicyNever, pod.Spec.RestartPolicy)
	require.NotNil(t, pod.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, int64(1800), *pod.Spec.ActiveDeadlineSeconds)
}

func TestBuildImageStepPod_EmptyEnv(t *testing.T) {
	pod := buildImageStepPod("r", "ci", "busybox", nil, 3600)
	assert.Empty(t, pod.Spec.Containers[0].Env)
	assert.Equal(t, "ucd-img-r-", pod.GenerateName)
}
