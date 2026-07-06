package agent

import (
	"runtime"

	"github.com/eirueimi/unified-cd/internal/api"
)

// agentOSForStep reports the OS a step actually runs on, for the
// UNIFIED_AGENT_OS env var. A uses-scope step or a step-level runsIn.image step
// executes in an isolated Linux container regardless of the agent's host OS, so
// it reports "linux"; every other step reports the host OS.
func agentOSForStep(step api.ClaimStep) string {
	if step.ScopeID != "" || (step.RunsIn != nil && step.RunsIn.Image != "") {
		return "linux"
	}
	return runtime.GOOS
}
