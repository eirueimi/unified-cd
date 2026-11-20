package k8sagent

import (
	"strings"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
)

// shQuote single-quotes s for a POSIX shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// clearWorkspaceScript returns the script that empties root without touching
// any volume mounted below it. The script lives in the agent package
// (agentlib.ClearWorkspaceScript), where the host agent's claim pod reuse
// shares it.
func clearWorkspaceScript(root string, vols []agentlib.MountedVolume) string {
	return agentlib.ClearWorkspaceScript(root, vols)
}
