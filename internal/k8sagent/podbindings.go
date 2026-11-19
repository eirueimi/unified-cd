package k8sagent

import (
	"sync"

	"github.com/eirueimi/unified-cd/internal/api"
)

// podBindingSet is a small mutex-guarded map from runID to the Kubernetes
// Pods (name + UID) executing it, mirroring internal/agent.RunSet's shape and
// lifecycle but carrying values instead of mere membership.
//
// A run is bound to its claim Pod and to every isolated scope Pod this agent
// created for it: each scope Pod runs its own artifact sidecar, which
// presents its own Pod's token, so the controller must know every one of them.
//
// This is what feeds the heartbeat's pod-binding report (see agent.go's Run):
// the controller learns "which Pods run which run" only through the heartbeat
// this set is snapshotted into, so its lifecycle must track a claim's exactly
// like activeRuns.RunSet does: Add once the claim's Pod is known (in
// executeRun, right after CreatePod/pool.ClaimPod returns), AddScope when a
// scope Pod is created and RemoveScope when it is deleted, Remove when the
// claim's dispatch goroutine returns (k8sClaimLoop, next to its
// activeRuns.Remove). A run never observed here by the time it terminates
// simply never had a binding to report.
type podBindingSet struct {
	mu sync.Mutex
	m  map[string]*runPodBindings

	// kick has capacity 1. Add and AddScope send on it without blocking, so
	// the heartbeat loop (agentlib.StartHeartbeatKicked) beats at once instead
	// of at the next tick: a new binding is what lets the Pod's sidecar get
	// presigned URLs, and the next tick may be 15s away. Several adds before
	// the loop wakes coalesce into one beat, which carries all of them. A kick
	// is not instant when the heartbeat goroutine is busy: it runs the volume
	// apply inside the beat, which can take up to volumeApplyTimeout (30s), so
	// the beat waits behind it.
	kick chan struct{}
}

// runPodBindings is one run's Pods: the claim Pod (nil until Add) and the
// scope Pods in creation order.
type runPodBindings struct {
	claim  *api.PodBinding
	scopes []api.PodBinding
}

// newPodBindingSet returns an empty podBindingSet ready to use.
func newPodBindingSet() *podBindingSet {
	return &podBindingSet{m: make(map[string]*runPodBindings), kick: make(chan struct{}, 1)}
}

func (s *podBindingSet) kickLocked() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *podBindingSet) runLocked(runID string) *runPodBindings {
	r := s.m[runID]
	if r == nil {
		r = &runPodBindings{}
		s.m[runID] = r
	}
	return r
}

// Add records that binding is the claim Pod executing runID. A nil receiver is
// a no-op: several existing tests build a *K8sAgent via a bare struct literal
// (bypassing NewK8sAgent, which is the only place that calls
// newPodBindingSet), and executeRun must keep working against those exactly
// as it did before this field existed rather than panic on a nil map.
func (s *podBindingSet) Add(runID string, binding api.PodBinding) {
	if s == nil {
		return
	}
	s.mu.Lock()
	b := binding
	s.runLocked(runID).claim = &b
	s.kickLocked()
	s.mu.Unlock()
}

// AddScope records an isolated scope Pod of runID as an additional binding.
// Adding a Pod already recorded (same UID) changes nothing. Nil-safe.
func (s *podBindingSet) AddScope(runID string, binding api.PodBinding) {
	if s == nil {
		return
	}
	s.mu.Lock()
	r := s.runLocked(runID)
	for _, p := range r.scopes {
		if p.PodUID == binding.PodUID {
			s.mu.Unlock()
			return
		}
	}
	r.scopes = append(r.scopes, binding)
	s.kickLocked()
	s.mu.Unlock()
}

// RemoveScope drops the scope Pod named podName from runID's bindings, called
// when the scope Pod is deleted. Nil-safe.
func (s *podBindingSet) RemoveScope(runID, podName string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.m[runID]
	if r == nil {
		return
	}
	kept := r.scopes[:0]
	for _, p := range r.scopes {
		if p.PodName != podName {
			kept = append(kept, p)
		}
	}
	r.scopes = kept
	if r.claim == nil && len(r.scopes) == 0 {
		delete(s.m, runID)
	}
}

// Kicks returns the channel Add signals on, for StartHeartbeatKicked. A nil
// receiver yields a nil channel, which never fires.
func (s *podBindingSet) Kicks() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.kick
}

// Remove drops every binding of runID, called once its claim has fully returned
// (mirrors RunSet.Remove). Nil-safe for the same reason as Add.
func (s *podBindingSet) Remove(runID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, runID)
}

// Snapshot returns one Pod per run (the claim Pod, else the first scope Pod) as
// a new map: the old single-binding report an older controller understands.
// Always non-nil.
func (s *podBindingSet) Snapshot() map[string]api.PodBinding {
	if s == nil {
		return map[string]api.PodBinding{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]api.PodBinding, len(s.m))
	for k, r := range s.m {
		switch {
		case r.claim != nil:
			out[k] = *r.claim
		case len(r.scopes) > 0:
			out[k] = r.scopes[0]
		}
	}
	return out
}

// SnapshotSets returns every bound Pod per run (claim Pod first, then scope
// Pods) as a new map: api.HeartbeatRequest.PodBindingSets. Always non-nil.
func (s *podBindingSet) SnapshotSets() map[string][]api.PodBinding {
	if s == nil {
		return map[string][]api.PodBinding{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]api.PodBinding, len(s.m))
	for k, r := range s.m {
		var pods []api.PodBinding
		if r.claim != nil {
			pods = append(pods, *r.claim)
		}
		pods = append(pods, r.scopes...)
		if len(pods) > 0 {
			out[k] = pods
		}
	}
	return out
}
