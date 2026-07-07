package agent

import (
	"github.com/eirueimi/unified-cd/internal/api"
)

// agentOSForStep reports the OS a step actually runs on, for the
// UNIFIED_AGENT_OS env var. A uses-scope step, a step-level runsIn.image step,
// or a runsIn.container step executes in an isolated Linux container
// regardless of backend, so it reports "linux"; every other step reports
// defaultOS (the backend's ExecBackend.DefaultAgentOS(), since this
// legitimately differs between the host agent (runtime.GOOS) and the k8s
// agent, which always execs into a Linux pod).
func agentOSForStep(step api.ClaimStep, defaultOS string) string {
	if step.ScopeID != "" || (step.RunsIn != nil && (step.RunsIn.Image != "" || step.RunsIn.Container != "")) {
		return "linux"
	}
	return defaultOS
}
