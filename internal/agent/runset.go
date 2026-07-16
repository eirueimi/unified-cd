package agent

import "sync"

// RunSet is a small mutex-guarded set of active run IDs. Both the host agent
// (agent.go) and the k8s agent (internal/k8sagent/agent.go, via this
// exported type) use one to track which runs are currently executing in this
// process, so the periodic heartbeat can carry them to the controller — the
// foundation for the controller's lost-claim reconcile: a live agent that
// heartbeats with an empty active set (or without a given run) is a
// candidate for reclaiming orphaned claims.
type RunSet struct {
	mu sync.Mutex
	m  map[string]struct{}
}

// NewRunSet returns an empty RunSet ready to use.
func NewRunSet() *RunSet {
	return &RunSet{m: make(map[string]struct{})}
}

// Add records id as active.
func (s *RunSet) Add(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = struct{}{}
}

// Remove drops id from the active set. Safe to call even if id was never
// added (e.g. a defer'd Remove after Add failed for some other reason).
func (s *RunSet) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

// Snapshot returns the current active IDs as a new, non-nil slice — empty
// (not nil) when no runs are active, so a heartbeat provider built on top of
// Snapshot always sends a body (see Client.Heartbeat).
func (s *RunSet) Snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.m))
	for id := range s.m {
		out = append(out, id)
	}
	return out
}
