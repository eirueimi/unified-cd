package agent

import (
	"context"
	"log/slog"
	"time"

	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

// Container labels the host agent puts on every claim-pod and scope container
// it creates, so a restarted agent with the same ID can find and remove the
// ones a crashed process left behind.
const (
	labelAgentID  = "unified-cd/agent-id"
	labelClaimPod = "unified-cd/claim-pod"
	labelScope    = "unified-cd/scope"
)

// sweepTimeout bounds the whole startup sweep so a wedged container runtime
// cannot stall Agent.Run before any slot starts.
var sweepTimeout = 2 * time.Minute // a var only so tests can shorten it

// claimPodLabels returns the labels for a claim-pod container (pause, sidecar
// or job); nil when agentID is empty (no sweep identity).
func claimPodLabels(agentID string) map[string]string {
	if agentID == "" {
		return nil
	}
	return map[string]string{labelAgentID: agentID, labelClaimPod: "true"}
}

// scopeLabels returns the labels for a uses-scope container; nil when agentID
// is empty.
func scopeLabels(agentID string) map[string]string {
	if agentID == "" {
		return nil
	}
	return map[string]string{labelAgentID: agentID, labelScope: "true"}
}

// sweepOrphanContainers removes every claim-pod or scope container labelled
// with agentID. It runs once at startup, before any slot starts, when no claim
// of this agent can be live: anything carrying the agent's label belongs to a
// previous process. It is a no-op when agentID is empty or the runtime does not
// implement crt.PodRuntime (Apple container). It returns the removed IDs.
func sweepOrphanContainers(ctx context.Context, rt crt.ContainerRuntime, agentID string) (removed []string) {
	pr, ok := rt.(crt.PodRuntime)
	if agentID == "" || !ok {
		slog.Info("startup container sweep skipped", "agentIDSet", agentID != "", "labelSupport", ok)
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()
	defer func() {
		if ctx.Err() == context.DeadlineExceeded {
			slog.Warn("startup container sweep timed out; continuing startup", "timeout", sweepTimeout.String(), "removed", len(removed))
		}
	}()
	infos, err := pr.ListContainers(ctx, map[string]string{labelAgentID: agentID})
	if err != nil {
		slog.Warn("startup container sweep: could not list containers", "error", err)
		return nil
	}
	for _, c := range infos {
		// Re-check the labels rather than trusting the runtime's filter: only a
		// container that is provably this agent's claim pod or scope is removed.
		if c.Labels[labelAgentID] != agentID {
			continue
		}
		if c.Labels[labelClaimPod] != "true" && c.Labels[labelScope] != "true" {
			continue
		}
		if err := rt.Remove(ctx, c.Handle); err != nil {
			slog.Warn("startup container sweep: could not remove container", "container", c.Handle.ID, "error", err)
			continue
		}
		removed = append(removed, c.Handle.ID)
	}
	return removed
}
