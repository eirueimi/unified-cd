package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSidecarLogIndex_DistinctFromStepsAndSystem(t *testing.T) {
	assert.Equal(t, 100000, SidecarLogIndex(0))
	assert.Equal(t, 100001, SidecarLogIndex(1))
	assert.Equal(t, 90000, ArtifactLogIndex)
	// Must not collide with real step indices [0,N) or System (-1).
	assert.Greater(t, SidecarLogIndex(0), 1000)
	assert.NotEqual(t, -1, SidecarLogIndex(0))
	assert.NotEqual(t, ArtifactLogIndex, SidecarLogIndex(0))
}

func TestSidecarContainerNames_SkipsJob_KeepsOrder(t *testing.T) {
	pt := &PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"name": "job", "image": "golang"},
		map[string]any{"name": "mysql", "image": "mysql:8"},
		map[string]any{"name": "redis", "image": "redis:7"},
	}}}
	assert.Equal(t, []string{"mysql", "redis"}, SidecarContainerNames(pt))
}

func TestSidecarContainerNames_NilAndEmpty(t *testing.T) {
	assert.Nil(t, SidecarContainerNames(nil))
	assert.Nil(t, SidecarContainerNames(&PodTemplate{}))
	// A template with only the job container has no sidecars.
	pt := &PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"name": "job", "image": "golang"},
	}}}
	assert.Nil(t, SidecarContainerNames(pt))
}
