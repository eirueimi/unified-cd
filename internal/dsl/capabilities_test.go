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

	// podTemplate carrying resources.requests -> pod (the host has no request
	// concept and silently drops it; see PodTemplateNeedsKubernetes).
	requestsPT := &PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"name": "job", "image": "python:3.12-slim",
			"resources": map[string]any{"requests": map[string]any{"cpu": "500m"}}},
	}}}
	assert.Equal(t, []string{"pod"}, RequiredCaps(Spec{PodTemplate: requestsPT}))

	// podTemplate carrying only literal env and cpu/memory-as-string
	// resources.limits -> container (both are host-honoured in full).
	hostRunnablePT := &PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"name": "job", "image": "python:3.12-slim",
			"env":       []any{map[string]any{"name": "MODE", "value": "fast"}},
			"resources": map[string]any{"limits": map[string]any{"cpu": "1", "memory": "2Gi"}}},
	}}}
	assert.Equal(t, []string{"container"}, RequiredCaps(Spec{PodTemplate: hostRunnablePT}))

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
