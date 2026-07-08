package agent

import (
	"runtime"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
)

// A step that runs in an isolated Linux container (a uses-scope, or a
// container: step) must report UNIFIED_AGENT_OS=linux regardless of the
// agent's host OS; every other step reports the backend's default OS.
func TestAgentOSForStep(t *testing.T) {
	assert.Equal(t, "linux", agentOSForStep(api.ClaimStep{ScopeID: "scope:build"}, runtime.GOOS),
		"a uses-scope step runs in a linux container")
	assert.Equal(t, "linux", agentOSForStep(api.ClaimStep{Container: "mysql"}, runtime.GOOS),
		"a container: step runs in a linux container")
	assert.Equal(t, runtime.GOOS, agentOSForStep(api.ClaimStep{}, runtime.GOOS),
		"a plain step reports the backend's default OS (runtime.GOOS on the host)")
}
