package k8sagent

import (
	"sync"

	"github.com/eirueimi/unified-cd/internal/api"
)

// podBindingSet is a small mutex-guarded map from runID to the Kubernetes
// Pod (name + UID) executing it, mirroring internal/agent.RunSet's shape and
// lifecycle but carrying a value instead of mere membership.
//
// This is what feeds StartHeartbeat's podBindings provider (see agent.go's
// Run): the controller learns "which Pod runs which run" only through the
// heartbeat this set is snapshotted into, so its lifecycle must track a
// claim's exactly like activeRuns.RunSet does — Add once the claim's Pod is
// known (in executeRun, right after CreatePod/pool.ClaimPod returns),
// Remove when the claim's dispatch goroutine returns (k8sClaimLoop, next to
// its activeRuns.Remove). A run never observed here by the time it
// terminates simply never had a binding to report, which is fine: nothing
// reads podBindingSet directly, only its periodic Snapshot.
type podBindingSet struct {
	mu sync.Mutex
	m  map[string]api.PodBinding
}

// newPodBindingSet returns an empty podBindingSet ready to use.
func newPodBindingSet() *podBindingSet {
	return &podBindingSet{m: make(map[string]api.PodBinding)}
}

// Add records that binding is the Pod executing runID. A nil receiver is a
// no-op: several existing tests build a *K8sAgent via a bare struct literal
// (bypassing NewK8sAgent, which is the only place that calls
// newPodBindingSet), and executeRun must keep working against those exactly
// as it did before this field existed rather than panic on a nil map.
func (s *podBindingSet) Add(runID string, binding api.PodBinding) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[runID] = binding
}

// Remove drops runID's binding, called once its claim has fully returned
// (mirrors RunSet.Remove). Nil-safe for the same reason as Add.
func (s *podBindingSet) Remove(runID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, runID)
}

// Snapshot returns the current bindings as a new map, safe for the caller to
// range over without holding s.mu — the same "copy under lock, hand back
// the copy" shape as RunSet.Snapshot. Always non-nil (even when empty) so a
// caller feeding it straight into api.HeartbeatRequest.PodBindings can tell
// "reported, zero runs" apart from "not reported at all" the same way
// HeartbeatRequest.ActiveRunIDs already does — though in practice an empty,
// non-nil map and a nil one marshal identically once `omitempty` drops
// both, so this distinction is for symmetry with RunSet rather than
// something the wire format currently uses.
func (s *podBindingSet) Snapshot() map[string]api.PodBinding {
	if s == nil {
		return map[string]api.PodBinding{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]api.PodBinding, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out
}
