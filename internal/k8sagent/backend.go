package k8sagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/secrets"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// scopeEntry is one scope key's in-flight or completed pod creation. The Once
// makes concurrent callers for the same key share a single attempt instead of
// each creating a pod — the old check-then-act let two steps both miss the
// cache, and the loser's pod was orphaned, since CloseScopes only deletes
// pods that made it into the map.
//
// name and err are written inside the Once and read by CloseScopes, which
// never calls Do and so has no happens-before edge from it. Both accesses are
// therefore taken under k8sBackend.scopesMu.
//
// interest/abandoned are how a creation attempt learns that nobody wants it
// any more. Every caller that joins the entry increments interest and drops
// it again when its own context ends or its call returns; when the count
// reaches zero the abandoned channel is closed, which cancels the in-flight
// attempt (see scopeCreateContext). This replaces an earlier attempt to infer
// "the run is over" from the KIND of the winning caller's context error,
// which was a proxy and a wrong one: a job-level timeoutMinutes
// (internal/agent/orchestrator.go) reaches every step context as
// DeadlineExceeded, indistinguishable from one step's private `timeout:`.
// Counting live callers needs no such inference — a step's own deadline
// removes exactly one caller, while anything that ends the run (job timeout,
// controller cancellation, agent shutdown) ends every caller's context and so
// necessarily drives the count to zero.
//
// done/claimed exist for the case abandonment leaves behind: interest can
// reach zero (abandonedClosed) WHILE the winner's once.Do closure is still
// running — the pod may not even be created yet, or WaitForPodRunning may
// still be in flight. A caller that arrives in that window must not join
// this entry (see ensureScopePod's lookup): joining would block in once.Do
// and inherit whatever result the abandoned attempt eventually produces,
// which has nothing to do with the newcomer's own (possibly perfectly
// healthy) context. done says whether that result has been recorded yet
// (name/err are only meaningful once done is true); a newcomer that finds
// abandonedClosed && !done installs a fresh entry for the key instead of
// joining.
//
// That newcomer path is also why claimed exists, decoupled from map
// identity. Every prior version of this cleanup ("claim by removing this
// entry from b.scopes under lock, but only if you're still the one it maps
// to" — see CloseScopes and createScopePod) used b.scopes[key] == e as the
// ownership test for "does anybody else already own deleting this entry's
// Pod". That test silently assumed the map slot is the entry's only path to
// eventual cleanup. It stops being true the moment a newcomer installs a
// fresh entry for an abandoned key: the OLD entry can keep running to
// completion — including succeeding — long after it has stopped being
// b.scopes[key], and nothing would ever look it up again to delete its Pod.
// claimed is an arbitrator independent of the map: whichever of {this entry's
// own createScopePod failure branch, its own once.Do closure noticing it is
// no longer the map's current occupant, CloseScopes} first transitions it
// from false to true (see claimEntry) is the one responsible for that
// entry's Pod, if it has one, and covers every reachable interleaving of
// those three. Map identity still decides whether an entry's key should be
// freed for a future retry; claimed alone decides who deletes the Pod.
//
// A fourth entry can also still be mid-install — CreatePod not yet returned,
// so it has no name yet — when CloseScopes sweeps past it; that window used
// to be a documented, tolerated gap (nothing to claim, nothing to name), but
// is now closed the same way the newcomer case above is: CloseScopes evicts
// such an entry from the map without claiming it (see CloseScopes), so if it
// goes on to succeed anyway, its own once.Do closure finds itself no longer
// the map's current occupant and self-deletes through the existing
// newcomer-eviction path below — one mechanism, two ways to trigger it.
//
// interest, abandonedClosed, done and claimed are guarded by
// k8sBackend.scopesMu; abandoned itself is assigned once at construction and
// only ever closed, so reading the channel needs no lock.
type scopeEntry struct {
	once sync.Once
	name string
	err  error
	// done is set true, together with name/err, once the once.Do closure has
	// recorded this entry's final result. See the struct doc comment.
	done bool

	interest        int
	abandoned       chan struct{}
	abandonedClosed bool
	// claimed is this entry's Pod-cleanup arbitrator. See claimEntry and the
	// struct doc comment.
	claimed bool
}

// claimEntry marks e as claimed for Pod-cleanup purposes and reports whether
// THIS call is the one that must act (delete e's Pod, if it has one) —
// true — or whether some other call already has — false. Must be called
// with scopesMu held; see the scopeEntry doc comment for why this exists
// independent of e's map membership.
func claimEntry(e *scopeEntry) bool {
	if e.claimed {
		return false
	}
	e.claimed = true
	return true
}

// k8sBackend is the ExecBackend implementation for the k8s agent. It owns the
// per-claim pod identity (the pooled/per-run pod's name and workspace mount
// path), the claim's scope-pod entries (lazily created on first uses-scope
// step; scopesMu guards scopes so concurrent steps sharing a uses-scope can
// safely race to create it — see scopeEntry), and the secret masker used by
// StepLogWriters.
type k8sBackend struct {
	a       *K8sAgent
	runID   string
	podName string
	// jobName is the claim's qualified job name (e.g. "team-a/build"), passed
	// as the sidecar's --job flag on every cache restore/save exec so cache
	// entries stay namespaced per job (mirrors hostBackend.jobName).
	jobName   string
	mountPath string

	scopesMu sync.Mutex
	scopes   map[string]*scopeEntry

	// scopeCtx/scopeCancel bound scope-pod CREATION to the claim rather than
	// to whichever step happened to ask for the scope first. See
	// scopeCreateContext for why creation must not inherit a step's context,
	// and CloseScopes for where scopeCancel fires.
	//
	// context.Background() as the root, exactly like sidecarPump's streams
	// (SetMasker): both are claim-lifetime work that must outlive per-step
	// cancellation, and both are ended explicitly by CloseScopes — which
	// RunClaim defers before anything can early-return
	// (internal/agent/orchestrator.go), so "claim-scoped" here is a
	// guarantee, not a hope.
	scopeCtx    context.Context
	scopeCancel context.CancelFunc
	// runCancelWatchOnce guards the claim's run-cancel watch, started lazily
	// by the first scope-pod creation. See startRunCancelWatch.
	runCancelWatchOnce sync.Once
	// runCancelWatchWG lets CloseScopes JOIN the watch goroutine after
	// cancelling it, mirroring RunClaim's own cancel poller
	// (internal/agent/orchestrator.go: "cancelRun(); pollerWG.Wait()"), which
	// this watch is explicitly modeled on. It exits on its own once
	// scopeCtx.Done() fires, so nothing here accumulates without a join —
	// but not joining means CloseScopes can return, and the claim's context
	// (agent.go's pod-teardown defer, a test's httptest server) can be torn
	// down, before the watch goroutine has actually observed scopeCtx.Done()
	// and stopped touching claim state.
	//
	// The join is BEST-EFFORT UNDER A DEADLINE: CloseScopes races it against
	// its teardown budget and gives up if that expires (see CloseScopes).
	// Wait takes no context, so a join that could not be abandoned would be
	// able to pin teardown past a ceiling the operator-facing docs promise.
	// The guarantee above therefore holds on every ordinary path and is
	// deliberately dropped at the deadline: what it protects against — a
	// straggler poll against a torn-down claim — costs a log line, where the
	// alternative costs the agent a concurrency slot forever.
	runCancelWatchWG sync.WaitGroup
	// runCancelWatchClosing is set under scopesMu by CloseScopes, BEFORE it
	// calls runCancelWatchWG.Wait(), and checked under the same lock by
	// startRunCancelWatch before it calls Add. This exists because Add must
	// happen-before the Wait it is meant to be seen by (textbook
	// sync.WaitGroup misuse otherwise), and startRunCancelWatch runs from
	// ensureScopePod's once.Do closure with no lock of its own — so without
	// this, a CloseScopes overlapping a claim's first scope creation could
	// have its Wait() observe a zero counter and return immediately, never
	// joining the watch goroutine.
	//
	// Serializing "is CloseScopes already winding down" and the Add behind
	// scopesMu gives that happens-before edge: either startRunCancelWatch's
	// critical section (check the flag, Add) completes — and is therefore
	// visible — before CloseScopes takes the lock to set the flag and then
	// calls Wait, or CloseScopes's flag is already set when
	// startRunCancelWatch looks, in which case it skips the Add entirely.
	// Skipping is harmless: CloseScopes always cancels b.scopeCtx before
	// setting this flag (see CloseScopes), so a watch started at that point
	// would have nothing left to do anyway. See startRunCancelWatch and
	// CloseScopes for the two sides of this handoff — do not move the Add
	// back into the goroutine-spawning closure without preserving this
	// ordering.
	runCancelWatchClosing bool

	masker *secrets.Masker

	// sidecarNames/claimSince/sidecarPump drive user-sidecar log streaming.
	// The pump is constructed (and started) lazily in SetMasker, once the
	// masker is known, and stopped in CloseScopes — mirroring the host
	// backend's pump ownership model (see backend_host.go). claimSince bounds
	// GetLogs' replay to this run's window so a reused pooled pod doesn't
	// re-emit a previous claim's sidecar output.
	sidecarNames []string
	claimSince   metav1.Time
	sidecarPump  *k8sSidecarPump
}

// newK8sBackend constructs the ExecBackend for one claim's executeRun call,
// after the run/pooled pod has been acquired and is Running. sidecarNames are
// the user podTemplate sidecar container names (declared order); claimSince is
// the claim's start time, used as the sidecar log stream's SinceTime. jobName
// is the claim's qualified job name, passed to the sidecar's cache
// restore/save subcommands via --job for per-job cache namespacing.
func newK8sBackend(a *K8sAgent, runID, jobName, podName, mountPath string, sidecarNames []string, claimSince metav1.Time) *k8sBackend {
	scopeCtx, scopeCancel := context.WithCancel(context.Background())
	return &k8sBackend{
		a: a, runID: runID, jobName: jobName, podName: podName, mountPath: mountPath,
		scopes:       map[string]*scopeEntry{},
		scopeCtx:     scopeCtx,
		scopeCancel:  scopeCancel,
		sidecarNames: sidecarNames, claimSince: claimSince,
	}
}

// RunDefault runs a step in the default (pooled/per-run) pod's container,
// threading step.Shell (the controller-resolved effective interpreter argv;
// nil means "apply the shim default") through to Executor.ExecStep.
func (b *k8sBackend) RunDefault(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return b.a.exec.ExecStep(ctx, b.podName, execContainer(step), script, step.Shell, env, stdout, stderr)
}

// envSliceToMap converts "KEY=VALUE" pairs (as produced by the orchestrator's
// already-template-expanded extraEnv) into a map. A malformed entry with no
// "=" is skipped defensively; the orchestrator never produces one.
func envSliceToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

// RunNamedContainer runs a step inside a specific named container of the
// default pod (runsIn.container), via the same ExecStep path as RunDefault,
// threading step.Shell through identically.
func (b *k8sBackend) RunNamedContainer(ctx context.Context, step api.ClaimStep, container, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return b.a.exec.ExecStep(ctx, b.podName, container, script, step.Shell, env, stdout, stderr)
}

// EnsureScope provisions (or reuses) the step's uses-scope pod, returning a
// ScopeHandle whose opaque payload is the scope pod's name.
func (b *k8sBackend) EnsureScope(ctx context.Context, step api.ClaimStep, env []string) (agentlib.ScopeHandle, error) {
	name, err := b.ensureScopePod(ctx, step, env)
	if err != nil {
		return agentlib.ScopeHandle{}, err
	}
	return wrapK8sScope(name), nil
}

// RunInScope execs script into the scope pod's "step" container.
//
// shell is the owning step's effective interpreter argv (agentlib.ExecBackend
// interface addition for the step-shell-shim feature; nil/empty means "apply
// the shim default"), threaded through to Executor.ExecStep exactly like
// RunDefault/RunNamedContainer thread step.Shell.
func (b *k8sBackend) RunInScope(ctx context.Context, h agentlib.ScopeHandle, script string, shell []string, env []string, stdout, stderr io.Writer) (int, error) {
	podName, ok := unwrapK8sScope(h)
	if !ok {
		return -1, fmt.Errorf("RunInScope: no scope handle")
	}
	return b.a.exec.ExecStep(ctx, podName, "step", script, shell, env, stdout, stderr)
}

// CloseScopes deletes every scope pod opened during the claim. It first stops
// the sidecar log pump (cancelling all sidecar streams and flushing remainders)
// so streams end before the pod is deleted/released — RunClaim defers
// CloseScopes and returns before agent.go's pod-teardown defer fires.
//
// ctx CARRIES THE TEARDOWN PHASE'S BUDGET — see agentlib.ExecBackend.CloseScopes
// for the contract, and agentlib.DefaultFinallyBudget for the four windows this
// is the fourth of. The two sites that would otherwise out-live it, the
// run-cancel watch join and the per-Pod DELETEs, honour it below; do not
// reintroduce context.WithoutCancel at either one.
//
// The one thing ctx does NOT bound here is sidecarPump.Stop's internal
// wg.Wait. Its stream goroutines unblock on the pump's own cancellation, but
// their closing Flush/reportStatus calls deliberately ride
// context.WithoutCancel so the last lines still ship — so Stop is bounded by
// the agent Client's 60s per-request HTTP timeout rather than by this window.
// That is a bounded overrun of seconds, not a wedge, and the host backend's
// Stop(ctx) has the identical shape: it is a property of the pump, shared by
// both backends, not a Kubernetes-side gap.
//
// It claims each entry it finds worth deleting (see claimEntry) before
// deleting its pod, rather than just reading name/err under the lock and
// deleting afterward. That claim is an ownership handoff with an in-flight
// ensureScopePod/createScopePod for the same key: e.name is written (in
// createScopePod) before e.err is known, so a CloseScopes racing with an
// in-flight attempt could otherwise see e.err == nil (its zero value, not yet
// set) and e.name != "" for a pod that is still waiting on
// WaitForPodRunning, or has just failed but whose once.Do closure has not yet
// recorded e.err, and issue its own DeletePod for the same pod
// createScopePod's own failure branch also deletes.
//
// The same claimEntry arbitration is used at every site that can delete a
// scopes entry's pod — here, createScopePod's WaitForPodRunning failure
// branch, and ensureScopePod's once.Do closure when an abandoned-and-replaced
// entry finishes after all. That symmetry matters: it is not enough for one
// site to avoid double-deleting a pod CloseScopes already claimed; a site
// that deletes unconditionally could instead race CloseScopes for the same
// entry and double-delete, or (before claimEntry existed) evict a DIFFERENT,
// live entry that a later caller installed for the same key after this one
// was removed — orphaning that entry's pod from CloseScopes just as surely as
// a double-delete wastes an API call. claimEntry replaced the map-identity
// version of this handoff (b.scopes[key] == e) specifically because that
// version assumed an entry's only path to eventual cleanup was staying
// b.scopes[key] — untrue once a newcomer can install a fresh entry for an
// abandoned key while the old one is still finishing (see the scopeEntry doc
// comment and ensureScopePod's lookup).
//
// This still removes every currently-mapped, not-yet-failed entry from
// b.scopes unconditionally (independent of who wins claimEntry) so the claim
// ends with an empty map regardless; claimEntry only decides who physically
// calls DeletePod.
//
// An entry can still be mid-install when this sweep runs — createScopePod
// installs it (via ensureScopePod's lookup) before it has a name, since name
// is only written once CreatePod returns (see createScopePod) — so the loop
// below cannot claim its Pod: there is no name yet to put in entries. This
// USED to mean skipping the entry outright, leaving it in b.scopes for the
// once.Do closure to find stillCurrent still true and take no action either
// (nothing else had touched the key), so a same-instant success — CreatePod
// and WaitForPodRunning both completing despite b.scopeCancel already having
// fired above — leaked the entry and its Pod, claimed by nobody. That was a
// real, if narrow, gap: TestK8sBackend_EnsureScope_StressCloseScopesRacesInFlightAttempts
// hits it under real (unchoreographed) timing on a loaded machine, not just
// in theory, which is what CI's "created 43, deleted 42" failure was.
//
// It is closed the same way the OTHER hole this arbitration exists for is
// closed — the newcomer-replaces-an-abandoned-entry case the scopeEntry doc
// comment describes: the loop below now evicts a mid-install entry from
// b.scopes too, without claiming it (there is nothing to claim yet). If that
// attempt goes on to fail, createScopePod's own failure branch cleans up as
// it always has (claimEntry there is still the first and only claimant). If
// it goes on to succeed, the once.Do closure computes stillCurrent as false —
// this sweep already evicted it — and falls into the EXISTING
// !stillCurrent-and-claimEntry self-delete branch below, the exact one the
// newcomer case already relies on. No new arbitration site, no new field:
// eviction alone is enough to hand ownership to the closure that already
// knows what to do with it.
//
// In production this race cannot actually happen regardless of the above:
// CloseScopes is deferred in the orchestrator and only runs after
// RunPipeline returns, and runParallel joins its goroutines before returning,
// so no in-flight ensureScopePod can overlap CloseScopes. That ordering lives
// in a different package (internal/agent/orchestrator.go) though, so this
// handoff does not rely on it — it stays correct even if a future refactor
// there changes it, and it is what the stress test above deliberately
// violates in order to exercise this arbitration at all.
func (b *k8sBackend) CloseScopes(ctx context.Context) {
	if b.sidecarPump != nil {
		b.sidecarPump.Stop()
	}
	// End the claim's scope-creation context. Creation is deliberately not
	// bound to any step's context (see scopeCreateContext), so this is what
	// stops an in-flight image pull once the claim is over — the same role
	// sidecarPump.Stop() plays for the sidecar streams above.
	if b.scopeCancel != nil {
		b.scopeCancel()
	}
	// Mark the watch as closing, under scopesMu, BEFORE joining it — this is
	// the other half of the Add/Wait ordering guarantee described on
	// runCancelWatchClosing. It must happen under the same lock
	// startRunCancelWatch checks, and it must happen before the Wait call
	// below, not after.
	b.scopesMu.Lock()
	b.runCancelWatchClosing = true
	b.scopesMu.Unlock()
	// Join the run-cancel watch (if it was ever started) before touching
	// anything else claim-scoped below — see runCancelWatchWG's doc comment.
	// A no-op wait if startRunCancelWatch was never called (no uses-scope
	// step in this claim, or b.a.client == nil).
	//
	// The join is raced against ctx, which is the teardown phase's budget
	// window (see ExecBackend.CloseScopes). sync.WaitGroup.Wait takes no
	// context and cannot be cancelled, so the join is moved onto a goroutine
	// and selected against ctx.Done(): without that, this single line could
	// pin teardown past its ceiling no matter what the caller's deadline
	// said, which is precisely the "documented bound the code does not keep"
	// this exists to avoid.
	//
	// Giving up on the join weakens — deliberately — the guarantee
	// runCancelWatchWG's doc comment describes (that CloseScopes does not
	// return while the watch may still touch claim state). That is the right
	// trade at the deadline: the watch only reads b.runID and calls
	// b.scopeCancel, which is idempotent and whose context is already
	// cancelled above, so a straggler observing a torn-down claim does
	// nothing worse than log; whereas a teardown that never returns holds the
	// agent's concurrency slot forever. On the ordinary path the join still
	// completes and the guarantee is unchanged.
	watchJoined := make(chan struct{})
	go func() {
		b.runCancelWatchWG.Wait()
		close(watchJoined)
	}()
	select {
	case <-watchJoined:
	case <-ctx.Done():
		slog.Warn("k8s: run-cancel watch did not stop within the teardown budget; continuing without joining it",
			"runId", b.runID, "error", ctx.Err())
	}

	b.scopesMu.Lock()
	entries := make(map[string]string, len(b.scopes))
	for key, e := range b.scopes {
		if e.err != nil {
			// Defensive, not a race window: the once.Do closure records
			// e.err and (when stillCurrent) removes itself from b.scopes
			// inside ONE uninterrupted critical section (see ensureScopePod)
			// — it never releases scopesMu in between. A failed entry that
			// WAS stillCurrent is therefore already gone from the map by the
			// time any later lock-holder, including this sweep, could look;
			// one that was NOT stillCurrent never occupied this key here to
			// begin with. e.err != nil should be unreachable in this loop;
			// skip it rather than assume that invariant if it somehow isn't.
			continue
		}
		if e.name != "" {
			if claimEntry(e) {
				entries[key] = e.name
			}
		}
		// Evict e from b.scopes regardless of whether it already has a name.
		// A named entry is claimed above (or already was, by someone else) and
		// is on its way out via entries. A not-yet-named entry — CreatePod
		// hasn't returned, so there is nothing here to claim — is evicted
		// anyway: that eviction IS this sweep's claim on it, just deferred.
		// See the doc comment above for why handing it to the existing
		// !stillCurrent self-delete branch this way, rather than adding a
		// second claimEntry call site, is enough to close that window.
		delete(b.scopes, key)
	}
	b.scopesMu.Unlock()

	// ctx, not context.WithoutCancel(ctx). The caller already hands us a
	// context that survives run cancellation AND carries the teardown budget
	// (see ExecBackend.CloseScopes); re-stripping it here would drop the
	// deadline with it and leave these DELETEs unbounded — a Kubernetes API
	// server that accepts connections but never answers would wedge teardown
	// forever, since no rest.Config.Timeout is set (and cannot be: the same
	// rest.Config drives exec streams and follow-mode log reads, which are
	// legitimately long-lived — see cmd/k8s-agent/main.go's buildRestConfig).
	//
	// If the budget expires mid-sweep the remaining DELETEs fail fast and
	// their Pods leak; runPodGC's label sweep (podgc.go) is the backstop, the
	// same one that covers the other documented leak paths here, and the
	// orchestrator records the truncation to the agent log.
	for key, name := range entries {
		if err := b.a.pm.DeletePod(ctx, name); err != nil {
			slog.Warn("k8s: failed to delete scope pod", "scopeKey", key, "pod", name, "error", err)
		}
	}
}

// ensureScopePod lazily creates (or returns the in-flight/completed) scope
// pod for a scoped step, keyed by scopeKey. Concurrent callers for the same
// key share a single scopeEntry and therefore a single sync.Once, so exactly
// one of them runs createScopePod; the rest block on Do until it finishes and
// then read the same result. Different keys get different entries and so
// proceed independently — scopesMu only ever guards the map/entry bookkeeping,
// never the pod creation itself.
//
// ctx is used ONLY to decide whether this caller's departure should abort the
// shared attempt (see scopeCreateContext); it never bounds the attempt.
//
// FIRST-CALLER-WINS ENV. env likewise comes from whichever caller won the
// Once, and the two callers do not agree on it: the orchestrator's run-step
// path passes the fully expanded extraEnv (UNIFIED_AGENT_OS,
// UNIFIED_WORKSPACE and every expanded `env:` value), while resolveScope —
// used by the cache:/uploadArtifact:/downloadArtifact: branches — passes nil
// (internal/agent/agent.go). A parallel: block mixing a cache: step and a
// run: step that share a ScopeID therefore builds the scope Pod with the full
// env or with only imageStepEnv(step), depending on which goroutine arrives
// first. The host backend has the identical shape
// (internal/agent/scope.go's scopeManager.ensure uses whichever caller holds
// the mutex first), so this is not a k8s-specific defect and it is not
// something this layer can fix: by the time either caller gets here the
// scope's env has already been decided by the DSL. What the concurrency flip
// changed is only the tie-break — declaration order became goroutine
// scheduling — so the symptom now reads as a flaky job rather than a stable
// misconfiguration. Tolerated deliberately; fixing it means making the env a
// property of the scope declaration rather than of the step that first
// touches it, which is a DSL change, not a backend one.
//
// JOINING AN ABANDONED ENTRY. A key's entry can have abandonedClosed true
// (every earlier caller has left) while its once.Do closure is STILL
// running — the winner's own context ending is what abandoned it, but the
// winner's call to createScopePod has not returned yet. A caller that looks
// up the key in that window must not join: e.once.Do would block until the
// abandoned attempt finishes and hand back whatever it produces (typically
// context.Canceled, from scopeCreateContext's route 1) to a caller whose OWN
// context may be perfectly healthy — see the k8s-scope-abandonment fix. The
// lookup below detects that window (abandonedClosed && !done) and installs a
// fresh entry instead, so this caller makes its own attempt. The abandoned
// entry's eventual result — success or failure — is still cleaned up
// correctly once it lands, via claimEntry rather than map identity; see the
// once.Do closure and the scopeEntry doc comment.
func (b *k8sBackend) ensureScopePod(ctx context.Context, step api.ClaimStep, env []string) (string, error) {
	key := scopeKey(step)

	b.scopesMu.Lock()
	e, ok := b.scopes[key]
	if ok && e.abandonedClosed && !e.done {
		// e is being torn down but has not finished — see the doc comment
		// above. Fall into the same "no live entry" branch below rather than
		// joining it.
		ok = false
	}
	if !ok {
		e = &scopeEntry{abandoned: make(chan struct{})}
		b.scopes[key] = e
	}
	// Register this caller's interest BEFORE releasing the lock, so there is
	// no window in which a freshly-installed entry looks abandoned.
	e.interest++
	b.scopesMu.Unlock()

	// Hold that interest for as long as this caller both exists and still
	// wants the scope. The watcher is not a one-shot decision: it runs for
	// the whole call, and the only two ways out are this caller returning
	// (stopInterestWatch) or this caller's context ending. Either way the
	// count drops exactly once, via releaseScopeInterest's sync.Once.
	stopInterestWatch := b.holdScopeInterest(ctx, e)
	defer stopInterestWatch()

	e.once.Do(func() {
		// Arm the authoritative run-cancel signal before the first Pod is
		// created (route 2 in scopeCreateContext). Once per backend.
		b.startRunCancelWatch()
		// The winner of the Once creates the pod for EVERY caller of this
		// key, so it must not run under its own step's deadline — see
		// scopeCreateContext.
		createCtx, stopCreateCtx := b.scopeCreateContext(e)
		defer stopCreateCtx()
		name, err := b.createScopePod(createCtx, step, env, e)
		b.scopesMu.Lock()
		e.name, e.err = name, err
		e.done = true
		// stillCurrent decides whether e's key should be freed for a future
		// retry, exactly as before. It does NOT decide who deletes e's Pod on
		// success — see below and the scopeEntry doc comment.
		stillCurrent := b.scopes[key] == e
		if err != nil {
			// Do not cache a failure. A later step needing this scope makes
			// its own attempt rather than inheriting an error it did not
			// cause; the callers waiting on THIS attempt still receive err
			// below, because they hold the entry pointer.
			//
			// Ownership check, same pattern as createScopePod's failure
			// branch and CloseScopes' claim: scopesMu is released between
			// createScopePod's own ownership-claiming delete/DeletePod call
			// and this lock acquisition, so by the time we get here a later
			// caller may already have removed this key (createScopePod's
			// branch, or a racing CloseScopes) and/or installed a brand new
			// entry for the same key (a later ensureScopePod call, once the
			// key was free — including the abandoned-entry replacement
			// above). An unconditional delete(b.scopes, key) would evict that
			// live successor instead of our own stale entry, orphaning ITS
			// pod from CloseScopes — the one thing this whole handoff exists
			// to prevent. Only delete if key still maps to this entry.
			if stillCurrent {
				delete(b.scopes, key)
			}
		}
		// e succeeded but is no longer the map's current occupant for its
		// key. Two distinct reasons land here: it was replaced (see
		// ensureScopePod's lookup, above) while still in flight, by a caller
		// that arrived after this entry was abandoned; or CloseScopes' own
		// sweep evicted it while it was still mid-install (no name yet to
		// claim) and the attempt went on to succeed anyway (see CloseScopes'
		// doc comment). Either way nobody will ever look e up again —
		// CloseScopes only walks the CURRENT map, and it has already run in
		// the second case — so without this, e's Pod would leak forever.
		// claimEntry (not stillCurrent) decides whether THIS call gets to
		// delete it, because CloseScopes can be racing the exact same
		// decision for the exact same e (see the doc comment on claimEntry).
		var selfDeleteName string
		if !stillCurrent && err == nil && name != "" && claimEntry(e) {
			selfDeleteName = name
		}
		b.scopesMu.Unlock()
		if selfDeleteName != "" {
			if delErr := b.a.pm.DeletePod(context.WithoutCancel(createCtx), selfDeleteName); delErr != nil {
				slog.Warn("k8s: failed to delete abandoned scope pod", "scopeKey", key, "pod", selfDeleteName, "error", delErr)
			}
		}
	})

	b.scopesMu.Lock()
	name, err := e.name, e.err
	b.scopesMu.Unlock()
	return name, err
}

// holdScopeInterest registers that callerCtx still wants entry e's scope, and
// returns a stop func the caller MUST defer. The interest is dropped exactly
// once - whichever comes first, the caller returning or its context ending.
//
// This is the mechanism that decides when an in-flight creation should be
// abandoned, and it deliberately measures rather than infers. "Does anyone
// still want this scope?" is answered by counting callers whose contexts are
// live; nothing anywhere reads the KIND of a context's error to guess whether
// the run is over.
func (b *k8sBackend) holdScopeInterest(callerCtx context.Context, e *scopeEntry) func() {
	var once sync.Once
	release := func() { once.Do(func() { b.releaseScopeInterest(e) }) }

	stopped := make(chan struct{})
	go func() {
		select {
		case <-stopped:
			// The caller returned; its deferred stop drops the interest.
		case <-callerCtx.Done():
			// This caller is gone - its step timed out, or the run ended. It
			// no longer wants the scope, but the OTHER callers on this key
			// may, so this only decrements; abandoning is what happens when
			// the count reaches zero.
			release()
		}
	}()

	return func() {
		close(stopped)
		release()
	}
}

// releaseScopeInterest drops one caller's interest in e and, if that was the
// last one, closes e.abandoned so the in-flight creation (if any) is
// cancelled.
//
// A caller arriving after abandonment joins an entry whose attempt is already
// being cancelled and will see that attempt's error. That interleaving needs
// every earlier caller's context to have ended first, which on a live run
// means the run itself is ending - and the failure is not cached, so the key
// is freed for a genuinely later step to retry from scratch.
func (b *k8sBackend) releaseScopeInterest(e *scopeEntry) {
	b.scopesMu.Lock()
	e.interest--
	abandon := e.interest <= 0 && !e.abandonedClosed
	if abandon {
		e.abandonedClosed = true
	}
	b.scopesMu.Unlock()
	if abandon {
		close(e.abandoned)
	}
}

// scopeCreateContext returns the context that bounds ONE scope-pod creation
// attempt, plus a stop func the caller MUST defer.
//
// Why creation cannot simply use the winning caller's context. The winner of
// scopeEntry.once creates the pod on behalf of every concurrent caller for
// that key, and the orchestrator hands each step a context carrying that
// step's own `timeout:` (or, for a retry: step, that attempt's timeout -
// internal/agent/orchestrator.go). Under Sequential that was harmless: there
// was only ever one caller. Under Concurrent, a `parallel:` block whose two
// members share a uses-scope would make the winner's private deadline
// everyone's deadline - member A with `timeout: 1` aborting a 90-second image
// pull, deleting the Pod, and handing its own DeadlineExceeded to member B,
// whose context was healthy and which would have waited the full
// PodStartTimeout. That is also a host/k8s divergence: the host's
// scopeManager.ensure (internal/agent/scope.go) caches only successes, so its
// second caller simply retries under its own context.
//
// So creation is rooted at the claim-scoped b.scopeCtx and bounded by
// Config.PodStartTimeout - the same operator knob that bounds the run Pod's
// own start wait. context.WithoutCancel of a caller's context would be worse
// than the bug: it would also survive run cancellation.
//
// Three routes abort the attempt, and NONE of them is a predicate on a
// context's error kind. Every one of them stays armed for the whole attempt;
// none can retire after a single decision:
//
//  1. e.abandoned - closed by releaseScopeInterest when the last caller that
//     still wanted this scope has gone. This is what catches a job-level
//     timeoutMinutes (which reaches every step context as DeadlineExceeded,
//     and so is invisible to any per-error-kind test), and equally a run
//     cancellation, an agent shutdown, or simply every interested step
//     failing for its own reasons. abandoned is a latching close, so this
//     select can neither miss the event nor consume it early.
//  2. b.scopeCtx - cancelled by startRunCancelWatch the moment the controller
//     reports the run terminal. That is the authoritative run-cancel signal,
//     the same one internal/agent's cancel poller and this package's
//     awaitPodRunning act on, read directly rather than inferred from the
//     error kind of a step context.
//  3. b.scopeCtx again - cancelled by CloseScopes at claim end.
//
// Accepted trade-off, and it is now narrow: a step whose `timeout:` fires
// while its SHARED scope Pod is still being created keeps waiting for that
// creation, up to PodStartTimeout, instead of failing at its own deadline -
// but ONLY while some sibling still wants the same Pod. A step that is the
// sole caller drops the interest count to zero and aborts at its own
// deadline, exactly as it did before this branch. A shared resource's
// creation cannot be bounded by one sharer's deadline without poisoning the
// others; PodStartTimeout is the knob that bounds it, and the overrun is
// documented for operators in
// docs/operator-manual/migrations/k8s-concurrent-step-execution.md.
func (b *k8sBackend) scopeCreateContext(e *scopeEntry) (context.Context, func()) {
	parent := b.scopeCtx
	if parent == nil {
		// Defensive: a k8sBackend assembled without newK8sBackend. Falling
		// back to Background keeps creation unbounded by any step, which is
		// the property that matters here.
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, b.a.cfg.PodStartTimeoutDuration())
	stopped := make(chan struct{})
	go func() {
		select {
		case <-stopped:
		case <-e.abandoned:
			cancel()
		}
	}()
	return ctx, func() {
		close(stopped)
		cancel()
	}
}

// startRunCancelWatch starts the claim's run-cancel watch, at most once per
// backend. It is called from the first scope-pod creation.
//
// It polls the controller for the run's authoritative status and cancels
// b.scopeCtx as soon as that status is terminal - the run was cancelled by an
// operator, or reaped. This is the real run-cancel event rather than a proxy
// for it: the same status, read from the same endpoint at the same interval,
// that internal/agent's own cancel poller (RunClaim) and this package's
// awaitPodRunning already act on. The watch cannot retire early - it runs
// until b.scopeCtx is done, which happens only when it cancels b.scopeCtx
// itself or CloseScopes does so at claim end.
//
// It starts on first scope creation rather than in SetMasker, even though
// SetMasker is where the claim's other background work (the sidecar log pump)
// is anchored, for one reason: the great majority of claims declare no
// uses-scope at all, and starting it there would double the controller's
// cancel-poll traffic for every one of them in order to watch for an event
// that could never matter. The state it acts on is still claim-scoped and
// still ended by CloseScopes, exactly like the pump.
//
// A nil client (a backend assembled directly in a test) skips the watch;
// routes 1 and 3 in scopeCreateContext are unaffected.
//
// The Add for runCancelWatchWG happens here under scopesMu, gated on
// runCancelWatchClosing, rather than unconditionally — see that field's doc
// comment on k8sBackend for why: it is what keeps this Add from racing
// CloseScopes' Wait on a zero counter.
func (b *k8sBackend) startRunCancelWatch() {
	if b.a == nil || b.a.client == nil || b.scopeCtx == nil || b.scopeCancel == nil {
		return
	}
	b.runCancelWatchOnce.Do(func() {
		b.scopesMu.Lock()
		if b.runCancelWatchClosing {
			// CloseScopes has already begun winding down (and, since it sets
			// this flag only after cancelling b.scopeCtx, the watch would
			// have nothing to do even if started). Do not Add — Once still
			// marks this as done, so no later call starts the watch either.
			b.scopesMu.Unlock()
			return
		}
		b.runCancelWatchWG.Add(1)
		b.scopesMu.Unlock()

		// Read the poll interval on this goroutine, not inside the watcher,
		// so a test mutating agentlib.CancelPollInterval never races the
		// watcher's read (mirrors awaitPodRunning and RunClaim's poller).
		pollInterval := agentlib.CancelPollInterval
		watchCtx := b.scopeCtx
		go func() {
			defer b.runCancelWatchWG.Done()
			ticker := time.NewTicker(pollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-watchCtx.Done():
					return
				case <-ticker.C:
					run, err := b.a.client.GetRun(watchCtx, b.runID)
					if err != nil {
						continue
					}
					if isTerminalRunStatus(run.Status) {
						slog.Info("k8s: run is terminal at the controller; aborting scope pod creation",
							"runId", b.runID, "status", run.Status)
						b.scopeCancel()
						return
					}
				}
			}
		}()
	})
}

// createScopePod creates one scope pod and waits for it to be Running. It is
// called at most once per scope key, from inside scopeEntry.once, under the
// claim-scoped context scopeCreateContext builds — NOT under the winning
// caller's step context.
//
// CreatePod and the Ready wait SHARE one budget: the same configurable knob
// as the run pod (Config.PodStartTimeout / UNIFIED_K8S_POD_START_TIMEOUT,
// resolved via Config.PodStartTimeoutDuration — see config.go and agent.go's
// awaitPodRunning), applied once, by scopeCreateContext, over the whole
// attempt. Under RestartPolicy: Never a pod stuck in
// Pending/ImagePullBackOff never transitions to Failed, so without this bound
// the wait would hang until the whole run is cancelled; this gives a bad
// scope image the same fast, explicit failure the run pod gets.
//
// The scope pod's env mirrors the expanded-env fix (commit 9e09c76): the
// orchestrator's already-template-expanded env (KEY=VALUE pairs) is merged
// over imageStepEnv(step)'s k8s-specific defaults, so the caller's expanded
// value wins over the raw, unexpanded step.Env map and a templated env value
// (e.g. {{ .Params.x }}) ships resolved rather than as the literal template.
func (b *k8sBackend) createScopePod(ctx context.Context, step api.ClaimStep, env []string, e *scopeEntry) (string, error) {
	key := scopeKey(step)
	envMap := imageStepEnv(step)
	for k, v := range envSliceToMap(env) {
		envMap[k] = v
	}
	// Only Limits ever reaches a claim (runsIn.resources.requests is rejected
	// at apply time — internal/dsl/parse.go's validateResources), so
	// Requests is intentionally left unset here; toResourceRequirements
	// handles a nil Limits (no resources: declared) as a no-op.
	resources := toResourceRequirements(&dsl.ResourceSpec{Limits: step.ScopeResourceLimits})
	pod := buildScopePod(b.runID, b.a.cfg.Namespace, step.ScopeID, step.ScopeImage, envMap,
		SidecarSpec{Image: b.a.cfg.SidecarImage, S3SecretName: b.a.cfg.SidecarS3SecretName, S3SecretMode: b.a.cfg.SidecarS3SecretMode, BrokerURL: b.a.cfg.Server}, b.a.cfg.ShimImage, resources)
	// attemptStart marks the beginning of the whole creation attempt
	// (CreatePod through WaitForPodRunning), not just the Running-wait below
	// — see the "pod creation abandoned after %s" message this feeds, which
	// names the attempt, not one phase of it.
	attemptStart := time.Now()
	created, err := b.a.pm.CreatePod(ctx, pod)
	if err != nil {
		// Residual, deliberately unfixed leak: if the API server actually
		// created the Pod but the client call still returned an error (a lost
		// response, or ctx being cancelled at exactly that instant), we return
		// before e.name is written and nothing in this claim can ever name
		// that Pod — CloseScopes skips entries with no name. Concurrency makes
		// it likelier, since N members now issue N CreatePod calls at once and
		// a cancellation hits all of them together. It is bounded, not
		// unbounded: buildScopePod labels every scope Pod
		// app=unified-cd-agent + unified-cd/runId=<runID> (scopepod.go), and
		// runPodGC sweeps exactly that label set against terminal runs
		// (podgc.go), so the orphan is reaped on the next GC pass. Closing it
		// here would mean a name-free List-by-label reconciliation on the
		// error path, which is more machinery than the GC already provides.
		return "", fmt.Errorf("uses-scope %q (image %q): create pod: %w", step.ScopeID, step.ScopeImage, err)
	}
	name := created.Name
	// Record the name as soon as the Pod exists, before waiting for Running,
	// so CloseScopes can always name what exists. From here until this
	// function returns, a real Pod is running in the cluster; if we only
	// wrote e.name on success (as ensureScopePod's caller does at the end),
	// a CloseScopes racing in while we are still in WaitForPodRunning below
	// would see an entry with no name and skip it, leaking the Pod.
	b.scopesMu.Lock()
	e.name = name
	b.scopesMu.Unlock()
	// Do NOT apply PodStartTimeout again here. scopeCreateContext already
	// bounds this whole attempt by it, so a second WithTimeout would never be
	// the earlier deadline and would only make the failure message below
	// claim a budget the wait did not actually have (it had PodStartTimeout
	// minus however long CreatePod took). waitBudget is read from the
	// context we were actually given, so the message reports the real one.
	waitBudget := b.a.cfg.PodStartTimeoutDuration()
	if deadline, ok := ctx.Deadline(); ok {
		waitBudget = time.Until(deadline)
	}
	if err := b.a.pm.WaitForPodRunning(ctx, name); err != nil {
		elapsed := time.Since(attemptStart)
		// Best-effort cleanup of the pod that never became ready. e.name was
		// already recorded above, but ensureScopePod deletes this whole entry
		// from b.scopes on failure (see the err != nil branch there), so a
		// CloseScopes that runs after we return would no longer find it
		// either — normally we must delete it ourselves here.
		//
		// Map removal and Pod-delete ownership are separate decisions here,
		// deliberately. Removing key from b.scopes is still keyed on identity
		// (stillCurrent): if some other entry has already taken the key —
		// CloseScopes claimed it, or a newcomer installed a fresh entry after
		// this one was abandoned (see ensureScopePod's lookup) — there is
		// nothing of ours left to remove. But whether THIS call deletes the
		// Pod is decided by claimEntry, not by stillCurrent: a CloseScopes
		// racing this exact failure (see its doc comment) may have already
		// claimed this entry and taken responsibility for the Pod even while
		// stillCurrent is still (momentarily) true, and a delete here would
		// then double up on CloseScopes' own delete.
		b.scopesMu.Lock()
		if b.scopes[key] == e {
			delete(b.scopes, key)
		}
		owned := claimEntry(e)
		b.scopesMu.Unlock()
		if owned {
			_ = b.a.pm.DeletePod(context.WithoutCancel(ctx), name)
		}
		// DeadlineExceeded means the attempt genuinely spent its whole
		// PodStartTimeout budget waiting — reporting that budget is accurate.
		// Any other error (context.Canceled from an abandoned/run-cancelled/
		// claim-torn-down attempt, or a real API error) did NOT spend that
		// budget, frequently by a wide margin: an attempt aborted 0.3s in
		// must not be reported as "did not become ready within 4m59.999s",
		// which names a budget it never had a chance to spend and reads as a
		// slow image pull rather than what actually happened.
		if errors.Is(err, context.DeadlineExceeded) {
			return "", fmt.Errorf("uses-scope %q (image %q): pod did not become ready within %s: %w", step.ScopeID, step.ScopeImage, waitBudget.Round(time.Millisecond), err)
		}
		if errors.Is(err, context.Canceled) {
			return "", fmt.Errorf("uses-scope %q (image %q): pod creation abandoned after %s: %w", step.ScopeID, step.ScopeImage, elapsed.Round(time.Millisecond), err)
		}
		return "", fmt.Errorf("uses-scope %q (image %q): pod did not become ready after %s: %w", step.ScopeID, step.ScopeImage, elapsed.Round(time.Millisecond), err)
	}
	return name, nil
}

// resolveSidecarTarget returns the sidecar container name and target pod name
// (empty = default pod) for a cache/artifact operation against scope. A
// scoped operation targets its scope pod's private scratch volume instead of
// the run pod's shared workspace; the mount path itself is resolved
// separately via ResolveArtifactPath/ResolveCachePath, so it is not part of
// this return.
func (b *k8sBackend) resolveSidecarTarget(ctx context.Context, scope agentlib.ScopeHandle) (sidecar, targetPod string, err error) {
	if scope.IsZero() {
		return artifactSidecarName, "", nil
	}
	podName, ok := unwrapK8sScope(scope)
	if !ok {
		return "", "", fmt.Errorf("resolveSidecarTarget: invalid scope handle")
	}
	return artifactSidecarName, podName, nil
}

// CacheRestore execs the unified-sidecar binary's "cache restore" into the
// target pod's sidecar. Best-effort: a miss, a failed restore, or an
// unreachable store never fails the run — but it is never reported as a HIT
// either. A non-zero sidecar exit and an absent UCD_CACHE_RESULT marker both
// come back as (false, err); the orchestrator only warns on that error (see
// orchestrator.go's cache step), so the run continues without a cache.
func (b *k8sBackend) CacheRestore(ctx context.Context, scope agentlib.ScopeHandle, key string, restoreKeys []string, path string) (bool, error) {
	sidecar, targetPod, err := b.resolveSidecarTarget(ctx, scope)
	if err != nil {
		return false, err
	}
	argv := []string{"unified-sidecar", "cache", "restore", "--key", key, "--path", path, "--job", b.jobName}
	for _, rk := range restoreKeys {
		argv = append(argv, "--restore-key", rk)
	}
	ec, stdout, err := b.sidecarExecArgvCapturingStdout(ctx, targetPod, sidecar, argv)
	if err != nil {
		return false, err
	}
	if ec != 0 {
		// The sidecar never reached the store at all — the dominant cause is a
		// missing/unset sidecarS3SecretName, where `unified-sidecar cache
		// restore` exits 1 with "cache requires S3 configuration (UNIFIED_S3_*)"
		// before contacting S3. Nothing was restored, so this must NOT be
		// reported as a hit. Returning an error (rather than a bare `false`)
		// keeps the distinction between "the store said miss" and "we could not
		// ask the store" visible in the log; it does not fail the step.
		return false, fmt.Errorf("cache restore %q: sidecar exited %d without restoring anything (see the unified-artifact sidecar's log for the reason)", key, ec)
	}
	// The exit code alone cannot distinguish hit from miss (the sidecar exits 0
	// for both), so the outcome comes from its UCD_CACHE_RESULT stdout marker.
	switch parseCacheResult(stdout) {
	case cacheResultHit:
		return true, nil
	case cacheResultMiss:
		return false, nil
	default:
		// Unknown, NEVER "hit". The sidecar emits a marker on every path it
		// reaches an answer on; the one exit-0 path that emits none is its
		// swallowed restore error ("cache restore error (ignored)"), i.e.
		// precisely a case where nothing was restored. (A pre-marker sidecar
		// image would also land here; agent and sidecar are required to be
		// upgraded in lockstep — see docs/operator-manual/operations.md.)
		return false, fmt.Errorf("cache restore %q: sidecar reported no UCD_CACHE_RESULT marker; treating the cache as not restored", key)
	}
}

// cacheResult is the tri-state outcome of a sidecar `cache restore`. The third
// state matters: an ABSENT marker is unknown, not a hit — reporting it as a hit
// is how a completely inert cache used to log "cache hit" indefinitely.
type cacheResult int

const (
	cacheResultUnknown cacheResult = iota
	cacheResultHit
	cacheResultMiss
)

// parseCacheResult reads the UCD_CACHE_RESULT marker from the sidecar's stdout.
// An absent marker yields cacheResultUnknown; callers must not treat that as a
// hit.
func parseCacheResult(stdout string) cacheResult {
	switch {
	case strings.Contains(stdout, "UCD_CACHE_RESULT=hit"):
		return cacheResultHit
	case strings.Contains(stdout, "UCD_CACHE_RESULT=miss"):
		return cacheResultMiss
	default:
		return cacheResultUnknown
	}
}

// CacheSave execs the unified-sidecar binary's "cache save" into the target
// pod's sidecar. Best-effort like CacheRestore — the returned error is only
// warned on by the deferred cache hook — but a sidecar that exited non-zero
// saved nothing and must not be logged as "cache saved".
func (b *k8sBackend) CacheSave(ctx context.Context, scope agentlib.ScopeHandle, key, path string, ttlDays int) error {
	sidecar, targetPod, err := b.resolveSidecarTarget(ctx, scope)
	if err != nil {
		return err
	}
	argv := []string{"unified-sidecar", "cache", "save", "--key", key, "--ttl-days", strconv.Itoa(ttlDays), "--path", path, "--job", b.jobName}
	ec, err := b.sidecarExecArgv(ctx, targetPod, sidecar, argv)
	if err != nil {
		return err
	}
	if ec != 0 {
		return fmt.Errorf("cache save %q: sidecar exited %d without saving anything (see the unified-artifact sidecar's log for the reason)", key, ec)
	}
	return nil
}

// UploadArtifact execs the unified-sidecar binary's "artifact upload" into
// the target pod's sidecar.
func (b *k8sBackend) UploadArtifact(ctx context.Context, scope agentlib.ScopeHandle, runID, name, path string) error {
	sidecar, targetPod, err := b.resolveSidecarTarget(ctx, scope)
	if err != nil {
		return err
	}
	argv := []string{"unified-sidecar", "artifact", "upload", "--run", runID, "--name", name, "--path", path}
	ec, err := b.sidecarExecArgv(ctx, targetPod, sidecar, argv)
	if err != nil {
		return err
	}
	if ec != 0 {
		return fmt.Errorf("artifact upload %q: sidecar exited %d", name, ec)
	}
	return nil
}

// DownloadArtifact execs the unified-sidecar binary's "artifact download"
// into the target pod's sidecar.
func (b *k8sBackend) DownloadArtifact(ctx context.Context, scope agentlib.ScopeHandle, runID, name, destDir string) error {
	sidecar, targetPod, err := b.resolveSidecarTarget(ctx, scope)
	if err != nil {
		return err
	}
	argv := []string{"unified-sidecar", "artifact", "download", "--run", runID, "--name", name, "--dest", destDir}
	ec, err := b.sidecarExecArgv(ctx, targetPod, sidecar, argv)
	if err != nil {
		return err
	}
	if ec != 0 {
		return fmt.Errorf("artifact download %q: sidecar exited %d", name, ec)
	}
	return nil
}

// sidecarExecArgv execs argv (no shell) into the sidecar container of
// targetPod (empty means the default pooled/run pod), shipping stderr via a
// LogPusher on the artifact sidecar's dsl.ArtifactLogIndex (its own identity,
// not step 0's stream) (mirroring the pre-refactor sidecarExec closure —
// cache/artifact steps have no per-step log stream of their own).
//
// Under concurrent step execution, two cache:/artifact steps running at once
// each build their own LogPusher over the SAME (runID, dsl.ArtifactLogIndex,
// "stderr") stream, and the controller assigns seq on arrival
// (internal/agent/runner.go), so their lines interleave inside the single
// "Artifacts/Cache" pseudo-step the UI renders
// (internal/controller/api_runs.go). Nothing is lost or corrupted — the
// pushers guard their own buffers — it is a readability cost, and only for
// jobs that run two cache/artifact steps concurrently. Fixing it would mean
// giving the pseudo-step a per-operation identity, which the logs table has
// no column for (see the matrix note in
// docs/operator-manual/migrations/k8s-concurrent-step-execution.md).
func (b *k8sBackend) sidecarExecArgv(ctx context.Context, targetPod, container string, argv []string) (int, error) {
	if targetPod == "" {
		targetPod = b.podName
	}
	stderrPusher := agentlib.NewLogPusher(b.a.client, b.a.cfg.AgentID, b.runID, dsl.ArtifactLogIndex, "stderr")
	stderrPusher.SetMasker(b.masker)
	ec, err := b.a.exec.ExecStepArgv(ctx, targetPod, container, argv, io.Discard, stderrPusher)
	stderrPusher.Flush(ctx)
	return ec, err
}

// sidecarExecArgvCapturingStdout is sidecarExecArgv but returns the sidecar's
// stdout (still shipping stderr to the log pusher). Used by CacheRestore to read
// the UCD_CACHE_RESULT marker.
//
// Its stderr pusher shares dsl.ArtifactLogIndex with every other cache/artifact
// operation in the claim, so concurrent steps multiplex onto one stream — see
// sidecarExecArgv for why that is accepted.
func (b *k8sBackend) sidecarExecArgvCapturingStdout(ctx context.Context, targetPod, container string, argv []string) (int, string, error) {
	if targetPod == "" {
		targetPod = b.podName
	}
	stderrPusher := agentlib.NewLogPusher(b.a.client, b.a.cfg.AgentID, b.runID, dsl.ArtifactLogIndex, "stderr")
	stderrPusher.SetMasker(b.masker)
	var stdout bytes.Buffer
	ec, err := b.a.exec.ExecStepArgv(ctx, targetPod, container, argv, &stdout, stderrPusher)
	stderrPusher.Flush(ctx)
	return ec, stdout.String(), err
}

// ResolveArtifactPath resolves p against the run/pooled pod's workspace mount
// path (non-scoped) or the scope pod's fixed working directory (scoped),
// mirroring the pre-refactor orchestrate's inline path.Join(mountPath, ...) /
// path.Join(scopeMountPath, ...) computation — now routed through
// agentlib.ContainWithinSlash so an absolute or escaping p is rejected
// instead of reaching outside the mount (e.g. the sidecar's mounted secrets).
func (b *k8sBackend) ResolveArtifactPath(scope agentlib.ScopeHandle, p string) (string, error) {
	if !scope.IsZero() {
		return agentlib.ContainWithinSlash(scopeMountPath, p)
	}
	return agentlib.ContainWithinSlash(b.mountPath, p)
}

// ResolveCachePath is identical to ResolveArtifactPath on k8s: a non-scoped
// cache path is resolved against the pod's mount path exactly like an
// artifact path (unlike the pre-fix host agent, which left it unresolved).
func (b *k8sBackend) ResolveCachePath(scope agentlib.ScopeHandle, p string) (string, error) {
	return b.ResolveArtifactPath(scope, p)
}

// WorkspacePath reports the cwd workspace root a step sees on this backend,
// for UNIFIED_WORKSPACE: the scope pod's fixed working directory when scope
// is non-zero, else the run/pooled pod's workspace mount path.
func (b *k8sBackend) WorkspacePath(scope agentlib.ScopeHandle) string {
	if !scope.IsZero() {
		return scopeMountPath
	}
	return b.mountPath
}

// DefaultAgentOS always reports "linux": every k8s exec path — including the
// "default pod" case — runs inside a Linux pod, unlike the host agent, which
// executes a non-scoped, non-runsIn.image step directly on its own OS.
func (b *k8sBackend) DefaultAgentOS() string {
	return "linux"
}

// RunPostHook runs a post: hook's script in scope's pod (the "step"
// container) when scope is non-zero, else in targetPod/container (the
// default pod's routing decided by the caller — container is meaningful on
// k8s, unlike the host backend, since a post hook must run in the same
// container the step body ran in). stdout/stderr are the owning step's
// shipping writers (see agentlib.ExecBackend.RunPostHook's doc comment); they
// replace what used to be a hardcoded io.Discard, io.Discard pair that threw
// post-hook output away.
//
// shell is the hook's effective interpreter argv (agentlib.ExecBackend
// interface addition for the step-shell-shim feature: post.Shell if the
// post: hook declared its own, else the owning step's effective
// ClaimStep.Shell — already resolved into this parameter by the
// orchestrator's hookStack, see orchestrator.go's hookShell computation),
// threaded through to Executor.ExecStep exactly like every other exec path.
func (b *k8sBackend) RunPostHook(ctx context.Context, scope agentlib.ScopeHandle, container, script string, shell []string, env []string, stdout, stderr io.Writer) error {
	targetPod := ""
	if !scope.IsZero() {
		podName, ok := unwrapK8sScope(scope)
		if !ok {
			return fmt.Errorf("RunPostHook: invalid scope handle")
		}
		targetPod = podName
		container = "step"
	}
	if targetPod == "" {
		targetPod = b.podName
	}
	_, err := b.a.exec.ExecStep(ctx, targetPod, container, script, shell, env, stdout, stderr)
	return err
}

// SetMasker installs the secret masker used by subsequently-created log
// writers (see StepLogWriters) and sidecar-exec stderr shipping. It also starts
// the user-sidecar log pump now that the masker is known, so sidecar logs are
// masked like every other stream (mirrors the host backend). GetLogs replays
// from claimSince regardless of when Start runs, so starting here — after
// secrets fetch — loses no history. Best-effort: if the underlying podManager
// is not a *PodManager (e.g. a test fake), streaming is skipped with a warning.
func (b *k8sBackend) SetMasker(m *secrets.Masker) {
	b.masker = m
	if len(b.sidecarNames) == 0 {
		return
	}
	pm, ok := b.a.pm.(*PodManager)
	if !ok {
		slog.Warn("k8s: sidecar log streaming skipped: podManager is not *PodManager", "runId", b.runID)
		return
	}
	b.sidecarPump = &k8sSidecarPump{
		client: pm.Client(), logs: b.a.client, ns: b.a.cfg.Namespace,
		pod: b.podName, agentID: b.a.cfg.AgentID, runID: b.runID,
		masker: b.masker, sidecars: b.sidecarNames, since: b.claimSince,
	}
	// context.Background(): the streams must outlive per-step cancellation;
	// Stop (called from CloseScopes) cancels them at claim end.
	b.sidecarPump.Start(context.Background())
}

// StepLogWriters returns LogPushers for stdout/stderr, auto-flushed for the
// step's duration, mirroring the host backend (internal/agent/backend_host.go).
// stdout used to ship via a per-line logLineWriter, one blocking HTTP request
// (and Store.AppendLog's two DB round trips) per line — the same pattern
// batch log ingestion removed from the controller's side but left standing
// here, since stderr already used a LogPusher. finish stops the auto-flush
// goroutine and does a final Flush of both streams.
func (b *k8sBackend) StepLogWriters(ctx context.Context, stepIndex int) (stdout, stderr io.Writer, finish func(ctx context.Context)) {
	stderrPusher := agentlib.NewLogPusher(b.a.client, b.a.cfg.AgentID, b.runID, stepIndex, "stderr")
	stderrPusher.SetMasker(b.masker)
	stdoutPusher := agentlib.NewLogPusher(b.a.client, b.a.cfg.AgentID, b.runID, stepIndex, "stdout")
	stdoutPusher.SetMasker(b.masker)

	flushCtx, stopAutoFlush := context.WithCancel(ctx)
	stderrPusher.StartAutoFlush(flushCtx, logAutoFlushInterval)
	stdoutPusher.StartAutoFlush(flushCtx, logAutoFlushInterval)

	finish = func(finishCtx context.Context) {
		stopAutoFlush()
		stderrPusher.Flush(finishCtx)
		stdoutPusher.Flush(finishCtx)
	}
	return stdoutPusher, stderrPusher, finish
}

// ConcurrencyMode reports how the k8s agent runs parallel-group / matrix
// members: concurrently, matching the standard agent.
//
// This returned Sequential until scope-pod creation became concurrency-safe.
// The old comment also blamed the hook stack, which was already wrong: the
// hook stack and postHooks live in the shared orchestrator
// (internal/agent/orchestrator.go) and have been guarded by postHooksMu since
// the standard agent went concurrent. Everything else the backend holds is
// either written once before the step loop (masker, sidecarPump, via
// SetMasker) or allocated per call (StepLogWriters), and the pod pool carries
// its own mutex.
func (b *k8sBackend) ConcurrencyMode() agentlib.ConcurrencyMode {
	return k8sConcurrencyMode
}

// k8sConcurrencyMode is the single source of truth for the k8s backend's
// step-concurrency mode. The in-package test doubles return it rather than
// repeating a literal — fakeK8sBackend (fakebackend_test.go), which every
// k8sagent orchestrator unit test drives, and parityK8sBackend
// (parity_k8s_test.go), which drives the k8s half of the parity suite. Both
// held a hardcoded agentlib.Sequential when this constant went Concurrent,
// so the flip shipped with no executable coverage anywhere outside a
// //go:build k8s test. Keeping the doubles pointed at this constant is what
// makes that impossible to repeat: change it here and every test that speaks
// for the k8s backend changes with it.
const k8sConcurrencyMode = agentlib.Concurrent

var _ agentlib.ExecBackend = (*k8sBackend)(nil)

// wrapK8sScope wraps a scope pod name as a ScopeHandle. An empty name yields
// the zero ScopeHandle (no scope / default location).
func wrapK8sScope(podName string) agentlib.ScopeHandle {
	if podName == "" {
		return agentlib.ScopeHandle{}
	}
	return agentlib.NewScopeHandle(podName)
}

// unwrapK8sScope extracts the scope pod name from a ScopeHandle produced by
// wrapK8sScope.
func unwrapK8sScope(h agentlib.ScopeHandle) (string, bool) {
	v, ok := agentlib.ScopeHandlePayload(h)
	if !ok {
		return "", false
	}
	name, ok := v.(string)
	return name, ok
}
