package k8sagent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestBuildImageStepPod_Resources(t *testing.T) {
	rs := &dsl.ResourceSpec{
		Requests: &dsl.ResourceList{CPU: "500m", Memory: "256Mi"},
		Limits:   &dsl.ResourceList{CPU: "1", Memory: "512Mi"},
	}
	pod := buildImageStepPod("r", "ci", "golang:1.22", nil, 3600, rs)
	c := pod.Spec.Containers[0]
	assert.True(t, c.Resources.Requests[corev1.ResourceCPU].Equal(resource.MustParse("500m")))
	assert.True(t, c.Resources.Requests[corev1.ResourceMemory].Equal(resource.MustParse("256Mi")))
	assert.True(t, c.Resources.Limits[corev1.ResourceCPU].Equal(resource.MustParse("1")))
	assert.True(t, c.Resources.Limits[corev1.ResourceMemory].Equal(resource.MustParse("512Mi")))
}

func TestBuildImageStepPod_NilResources(t *testing.T) {
	pod := buildImageStepPod("r", "ci", "busybox", nil, 3600, nil)
	c := pod.Spec.Containers[0]
	require.Empty(t, c.Resources.Requests)
	require.Empty(t, c.Resources.Limits)
}
