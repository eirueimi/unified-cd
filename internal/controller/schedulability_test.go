package controller

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
)

func agent(caps, labels []string) api.AgentInfo { return api.AgentInfo{Capabilities: caps, Labels: labels} }

func TestEvaluateSchedulability(t *testing.T) {
	host := agent([]string{"native", "container"}, []string{"kind:docker", "hostname:h1"})
	k8s := agent([]string{"pod", "container"}, []string{"kind:k8s", "kubernetes"})

	// native job, only a k8s agent online -> unsatisfiable by cap
	s := EvaluateSchedulability(dsl.Spec{Native: true}, []api.AgentInfo{k8s})
	assert.False(t, s.Satisfiable)
	assert.Contains(t, s.Reason, "native")

	// native job, host agent online -> satisfiable
	s = EvaluateSchedulability(dsl.Spec{Native: true}, []api.AgentInfo{host, k8s})
	assert.True(t, s.Satisfiable)

	// label no agent has -> unsatisfiable by label
	s = EvaluateSchedulability(dsl.Spec{Native: true, AgentSelector: []string{"kind:macos"}}, []api.AgentInfo{host})
	assert.False(t, s.Satisfiable)
	assert.Contains(t, s.Reason, "kind:macos")

	// param-templated selector -> don't warn; flag it
	s = EvaluateSchedulability(dsl.Spec{Native: true, AgentSelector: []string{"hostname:{{ .Params.agent }}"}}, []api.AgentInfo{host})
	assert.True(t, s.SelectorDependsOnParams)
	assert.True(t, s.Satisfiable) // cap part is satisfiable; label part deferred
}
