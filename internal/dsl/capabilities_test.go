package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRequiredCaps(t *testing.T) {
	assert.Equal(t, []string{"native"}, RequiredCaps(Spec{Native: true}))
	assert.Equal(t, []string{"container"}, RequiredCaps(Spec{}))

	// host-runnable podTemplate (plain name/image) -> container
	hostPT := &PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"name": "mysql", "image": "mysql:8"},
	}}}
	assert.Equal(t, []string{"container"}, RequiredCaps(Spec{PodTemplate: hostPT}))

	// k8s-only podTemplate (named agent template) -> pod
	k8sPT := &PodTemplate{Name: "golang", Spec: map[string]any{"containers": []any{
		map[string]any{"name": "job", "image": "golang:1.22"},
	}}}
	assert.Equal(t, []string{"pod"}, RequiredCaps(Spec{PodTemplate: k8sPT}))

	// native takes precedence even if a podTemplate is somehow present
	assert.Equal(t, []string{"native"}, RequiredCaps(Spec{Native: true, PodTemplate: hostPT}))
}

func TestValidCapability(t *testing.T) {
	assert.True(t, ValidCapability("native"))
	assert.True(t, ValidCapability("container"))
	assert.True(t, ValidCapability("pod"))
	assert.False(t, ValidCapability("gpu"))
	assert.False(t, ValidCapability(""))
}
