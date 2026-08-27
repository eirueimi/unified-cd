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
	// names, not just their counts, so it has real teeth for a LEAK: a name
	// present in created but absent from deleted fails it immediately.
	//
	// It does NOT, despite reading that way, exercise the "no double-deletes"
	// half of claimEntry's job: every attempt above (both phases) is joined
	// — wg.Wait()/epWG.Wait() — and the "must still be obtainable" loop above
	// this comment also runs to completion, before CloseScopes is ever
	// called. Nothing here is in flight when CloseScopes claims/sweeps, so
	// the CloseScopes-vs-in-flight-attempt race claimEntry exists to
	// arbitrate never happens in this test. A double-delete of one Pod
	// masking a leak of another would still make this assertion fail (the
	// multiset would gain a duplicate and lose an entry), but only a test
	// that actually lands a double-delete can prove that side of the
	// assertion has teeth — confirmed by fault injection: weakening
	// claimEntry to always claim leaves this assertion green across repeated
	// runs. TestK8sBackend_EnsureScope_StressCloseScopesRacesInFlightAttempts
	// (below) and the deterministic
	// TestK8sBackend_CloseScopes_DoesNotDoubleDeleteRacingFailedCreate are
	// what actually race CloseScopes against an in-flight attempt.
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

// TestK8sBackend_EnsureScope_StressCloseScopesRacesInFlightAttempts is the
// coverage the sibling stress test's final assertion reads as providing but
// does not: CloseScopes there only ever runs after every attempt has fully
// quiesced, so nothing races it. This test drives CloseScopes concurrently
// with many still-in-flight ensureScopePod attempts across many keys, using
// realistic (unchoreographed) timing via fakePM.waitJitter rather than
// gates, so the exact window claimEntry exists for — CloseScopes claiming an
// entry the same instant createScopePod's own failure branch (or its
// once.Do closure's self-delete) is trying to claim it — actually happens,
// repeatedly, across many runs.
//
// Verified to have teeth the same way the reviewer verified the underlying
// claimEntry arbitration: weakening claimEntry to always claim (so two
// arbitrators can both believe they own a Pod) reliably fails this test's
// final assertion with duplicate names in deleted; restoring claimEntry
// makes it pass again.
//
// This test also caught the narrow, then-still-open leak CI hit on main (a
// "created 43, deleted 42" failure): CloseScopes' sweep used to
// skip an entry outright if it was still mid-install (no name yet), leaving
// it owned by nobody if that attempt went on to succeed anyway despite
// b.scopeCancel already having fired. CloseScopes now evicts such an entry
// from b.scopes instead of skipping it, which is enough to route that case
// through the SAME !stillCurrent self-delete branch the newcomer-replaces-
// an-abandoned-entry case already used — see CloseScopes' doc comment. This
// test is what continues to hold that closed: it is the only one that
// actually lands CloseScopes concurrently with a mid-install (not-yet-named)
// attempt under real timing, rather than a choreographed one.
func TestK8sBackend_EnsureScope_StressCloseScopesRacesInFlightAttempts(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test; skipped under -short")
	}

	pm := &fakePM{waitJitter: 5 * time.Millisecond}
	a := &K8sAgent{cfg: Config{Namespace: "default", PodStartTimeout: "2s"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	const workers = 60
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			// A handful of shared keys, not one-per-worker: with only ~20
			// keys and 60 workers, most keys see several concurrent callers,
			// which is what actually exercises createScopePod's failure
			// branch and the once.Do closure's self-delete (both need a
			// SHARED entry), not just plain single-caller creation.
			step := api.ClaimStep{ScopeID: fmt.Sprintf("scope:close-race-%d", seed%20), ScopeImage: "golang:1.22"}
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(r.Intn(6))*time.Millisecond)
			defer cancel()
			_, _ = b.EnsureScope(ctx, step, nil)
		}(int64(i) + 1)
	}

	// Give the workers a moment to actually be in flight — some already past
	// CreatePod and parked in the jittered WaitForPodRunning — before
	// CloseScopes claims/cancels/sweeps concurrently with them. This is
	// deliberately a sleep, not a gate: the point is to land CloseScopes
	// inside the real, unchoreographed window, the same kind of interleaving
	// production scheduling could produce, rather than pinning one exact
	// instant.
	time.Sleep(2 * time.Millisecond)
	require.NotPanics(t, func() { b.CloseScopes(context.Background()) })

	wg.Wait()

	// Same exact-multiset check as the sibling stress test, but this time it
	// is actually covering what its comment describes: every Pod created
	// while genuinely racing CloseScopes must still be deleted exactly once.
	pm.mu.Lock()
	created := append([]string(nil), pm.createdNames...)
	deleted := append([]string(nil), pm.deleted...)
	pm.mu.Unlock()
	sort.Strings(created)
	sort.Strings(deleted)
	t.Logf("close-scopes race: created %d pod(s), deleted %d", len(created), len(deleted))
	assert.Equal(t, created, deleted,
		"every Pod created while racing CloseScopes against in-flight attempts must be deleted exactly once — no leaks, no double-deletes")
}
