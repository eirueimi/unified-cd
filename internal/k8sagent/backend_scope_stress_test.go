package k8sagent

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestK8sBackend_EnsureScope_StressInterestAndAbandonment hammers the
// interest-counting/abandonment machinery (scopeEntry.interest/abandoned)
// together with the abandoned-entry replacement and claimEntry generation
// check added for finding D1, across many goroutines and many keys sharing
// one backend. It complements
// TestK8sBackend_EnsureScope_JoinDuringAbandonmentDoesNotInheritCancellation,
// which pins one specific, deterministic interleaving: this soaks many
// random orderings so -race gets many independent chances to catch a
// bookkeeping bug in interest++/-- or in claimEntry racing CloseScopes/
// createScopePod's failure branch/the once.Do closure's self-delete.
//
// Two phases run against the SAME backend (so both funnel through the same
// scopesMu/claimEntry state, and the final leak/double-delete check below
// covers everything either phase created):
//
//  1. High contention on a handful of SHARED keys — many goroutines racing
//     to join/leave the same few entries, most of which resolve to a cached
//     success almost immediately (by design: a settled key should not keep
//     re-abandoning). This mainly stresses raw interest++/-- concurrency.
//  2. Many small "episodes" on FRESH, per-episode keys, each shaped like the
//     deterministic reproduction test but with real (unchoreographed) timing:
//     a few short-jittered-deadline callers race a straggler that arrives
//     slightly later with a healthy, never-cancelled context. Fresh keys mean
//     no prior success to short-circuit into a cache hit, so this phase is
//     what actually drives the entry through real abandon-while-in-flight
//     windows, repeatedly, under randomized scheduling.
func TestK8sBackend_EnsureScope_StressInterestAndAbandonment(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test; skipped under -short")
	}

	pm := &fakePM{waitJitter: 3 * time.Millisecond}
	a := &K8sAgent{cfg: Config{Namespace: "default", PodStartTimeout: "2s"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	var allSteps []api.ClaimStep

	// Phase 1: high contention on a handful of shared keys.
	sharedKeys := []api.ClaimStep{
		{ScopeID: "scope:a", ScopeImage: "golang:1.22"},
		{ScopeID: "scope:b", ScopeImage: "node:22"},
		{ScopeID: "scope:c", ScopeImage: "python:3.12"},
	}
	allSteps = append(allSteps, sharedKeys...)

	const workers = 40
	const roundsPerWorker = 15

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			for round := 0; round < roundsPerWorker; round++ {
				step := sharedKeys[r.Intn(len(sharedKeys))]
				var ctx context.Context
				var cancel context.CancelFunc
				if r.Intn(3) == 0 {
					ctx, cancel = context.WithTimeout(context.Background(), time.Duration(r.Intn(4))*time.Millisecond)
				} else {
					ctx, cancel = context.WithCancel(context.Background())
				}
				_, _ = b.EnsureScope(ctx, step, nil)
				cancel()
			}
		}(int64(i) + 1)
	}
	wg.Wait()

	// Phase 2: many small episodes, each on a fresh key, each shaped to
	// actually land callers inside the abandon-while-in-flight window.
	const episodes = 80
	episodeSteps := make([]api.ClaimStep, episodes)
	var epWG sync.WaitGroup
	for ep := 0; ep < episodes; ep++ {
		step := api.ClaimStep{ScopeID: fmt.Sprintf("scope:stress-%d", ep), ScopeImage: "golang:1.22"}
		episodeSteps[ep] = step
		epWG.Add(1)
		go func(seed int64, step api.ClaimStep) {
			defer epWG.Done()
			r := rand.New(rand.NewSource(seed))

			// A handful of short, jittery-deadline callers — standing in for
			// the winner (and possibly other siblings) whose own `timeout:`
			// fires while the shared attempt is still in flight. Each gets
			// its own *rand.Rand (math/rand.Rand is not safe for concurrent
			// use) rather than sharing the episode's r.
			var impatientWG sync.WaitGroup
			impatient := 2 + r.Intn(2) // 2 or 3
			for k := 0; k < impatient; k++ {
				impatientWG.Add(1)
				go func(kr *rand.Rand) {
					defer impatientWG.Done()
					ctx, cancel := context.WithTimeout(context.Background(), time.Duration(kr.Intn(3))*time.Millisecond)
					defer cancel()
					_, _ = b.EnsureScope(ctx, step, nil)
				}(rand.New(rand.NewSource(seed*1000 + int64(k) + 1)))
			}

			// The straggler: arrives slightly later, with a healthy,
			// never-cancelled context — the one whose result must never be
			// sacrificed to the impatient callers' abandonment.
			time.Sleep(time.Duration(r.Intn(3)) * time.Millisecond)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := b.EnsureScope(ctx, step, nil)
			assert.NoError(t, err, "the straggler's healthy context must not be sacrificed to an abandoned attempt (episode %s)", step.ScopeID)

			impatientWG.Wait()
		}(int64(ep) + 1000, step)
	}
	epWG.Wait()
	allSteps = append(allSteps, episodeSteps...)

	// Whatever abandonment/replacement churn the short-deadline callers
	// caused, every key touched must still be obtainable by a caller with a
	// healthy, never-cancelled context: no key may be left permanently
	// poisoned by a cached failure, and no healthy caller may be left hanging
	// on a generation that was already abandoned.
	for _, step := range allSteps {
		_, err := b.EnsureScope(context.Background(), step, nil)
		assert.NoError(t, err, "scope %q must still be obtainable after the stress round", step.ScopeID)
	}

	require.NotPanics(t, func() { b.CloseScopes(context.Background()) })

	// Every Pod fakePM ever created must have been accounted for by the end:
	// either deleted along the way (an abandoned/failed attempt's own
	// cleanup, or a self-delete by an evicted-but-succeeded attempt — see
	// claimEntry) or deleted just now by the CloseScopes call above. This
	// compares the exact (sorted) multiset of created names against deleted
	// names, not just their counts, so it catches a double-delete of one Pod
	// masking a leak of another just as surely as a plain leak.
	pm.mu.Lock()
	created := append([]string(nil), pm.createdNames...)
	deleted := append([]string(nil), pm.deleted...)
	pm.mu.Unlock()
	sort.Strings(created)
	sort.Strings(deleted)
	t.Logf("stress round created %d pod(s), deleted %d", len(created), len(deleted))
	assert.Equal(t, created, deleted,
		"every Pod created during the stress round must be deleted exactly once, by the time CloseScopes has run — no leaks, no double-deletes")
}
