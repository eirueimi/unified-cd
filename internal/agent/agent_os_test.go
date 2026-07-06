package agent

import (
	"runtime"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
)

// A step that runs in an isolated Linux container (a uses-scope, or a
// step-level runsIn.image) must report UNIFIED_AGENT_OS=linux regardless of the
// agent's host OS; every other step reports the host OS.
func TestAgentOSForStep(t *testing.T) {
	assert.Equal(t, "linux", agentOSForStep(api.ClaimStep{ScopeID: "scope:build"}, runtime.GOOS),
		"a uses-scope step runs in a linux container")
	assert.Equal(t, "linux", agentOSForStep(api.ClaimStep{RunsIn: &dsl.RunsIn{Image: "golang:1.22"}}, runtime.GOOS),
		"a step-level runsIn.image step runs in a linux container")
	assert.Equal(t, runtime.GOOS, agentOSForStep(api.ClaimStep{}, runtime.GOOS),
		"a plain step reports the backend's default OS (runtime.GOOS on the host)")
	assert.Equal(t, runtime.GOOS, agentOSForStep(api.ClaimStep{RunsIn: &dsl.RunsIn{Container: "named"}}, runtime.GOOS),
		"runsIn.container (no image) is not an isolated-image step")
}
