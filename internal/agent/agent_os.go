package agent

import (
	"github.com/eirueimi/unified-cd/internal/api"
)

// agentOSForStep reports the OS a step actually runs on, for the
// UNIFIED_AGENT_OS env var. A uses-scope step or a container: step executes
// in a Linux container regardless of backend, so it reports "linux"; every
// other step reports defaultOS (ExecBackend.DefaultAgentOS() — the host
// backend itself reports "linux" for an isolated claim, runtime.GOOS for a
// native one; k8s always "linux").
func agentOSForStep(step api.ClaimStep, defaultOS string) string {
	if step.ScopeID != "" || step.Container != "" {
		return "linux"
	}
	return defaultOS
}
