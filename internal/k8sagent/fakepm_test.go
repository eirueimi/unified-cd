package k8sagent

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// fakePM and fakeExec are the shared k8sagent test fixtures (a fake podManager
// and a fake step executor) used across the backend/scope/orchestrate suites.
// They record the pod they created and the exec call they received so tests can
// assert on the pod spec and exec target.
type fakePM struct {
	mu          sync.Mutex
	createCount int

	created   *corev1.Pod
	createdNm string
	// createdNames is every Pod name CreatePod has ever produced, in order —
	// unlike createdNm (the latest only), this lets a test check for an
	// exact, dup-free correspondence with deleted (below) across many
	// concurrent attempts, e.g. the stress test.
	createdNames []string
	waitErr      error
	// blockFirstWait, when non-nil, blocks only the FIRST WaitForPodRunning
	// call until it is closed; later calls return immediately. That asymmetry
	// is what lets a test prove a second scope key does not queue behind the
	// first key's pod wait: under a single lock held across creation, the
	// second caller never reaches WaitForPodRunning at all.
	blockFirstWait chan struct{}
	// waitStarted is closed by that first call, so a test can wait until one
	// goroutine is definitely parked before starting the second. Without it
	// the test races on which goroutine arrives first, and whichever did
	// would be the one blocked.
	waitStarted     chan struct{}
	waitCount       int
	deleted         []string
	waitHadDeadline bool
	waitCtxSeen     bool
	waitDeadline    time.Time // the actual deadline WaitForPodRunning's ctx carried, when waitHadDeadline is true

	// failWaitCalls, when > 0, limits waitErr to the first failWaitCalls
	// calls to WaitForPodRunning; later calls succeed (nil) regardless of
	// waitErr. 0 (the default) preserves the plain "every call returns
	// waitErr" behavior the other scope-race tests rely on. Used to make one
	// attempt for a scope key fail while a later attempt for the SAME key
	// (after the first's entry is gone) succeeds, which is what widens the
	// logic-race window between a failed attempt's ownership-claiming delete
	// and its DeletePod call into something a test can land a second,
	// successful attempt inside of.
	failWaitCalls int
	// ignoreCtxUntilGate, when true, makes the FIRST WaitForPodRunning call
	// block purely on blockFirstWait, ignoring ctx.Done() entirely, instead
	// of racing the two (the default select{} below). It exists to mimic the
	// REAL PodManager.WaitForPodRunning, which only checks ctx.Done() between
	// its 500ms polls rather than reacting to cancellation instantly
	// (podmanager.go) — so a test can cancel/abandon the caller while this
	// call is parked, and keep it parked for as long as it wants (simulating
	// "still polling, hasn't noticed cancellation yet"), rather than having
	// it race off the moment cancellation happens. The call's eventual return
	// value once the gate opens is still governed by waitErr/failWaitCalls
	// exactly as normal — it does not itself inspect ctx after the gate
	// opens, so the test controls the outcome directly.
	ignoreCtxUntilGate bool
	// waitJitter, when nonzero, makes EVERY WaitForPodRunning call sleep a
	// random duration in [0, waitJitter) before returning, honoring ctx.Done()
	// while it does so. It exists for stress tests that want realistic timing
	// variance across many concurrent attempts sharing a handful of keys —
	// some callers' short deadlines genuinely racing a creation still in
	// flight — without hand-choreographing every interleaving via gates.
	waitJitter time.Duration

	deleteCount int
	// blockFirstDelete, when non-nil, blocks only the FIRST DeletePod call
	// until it is closed; later calls return immediately. Mirrors
	// blockFirstWait/waitStarted below, but for DeletePod: it widens the
	// window between createScopePod's failure branch removing its own entry
	// from b.scopes and that same goroutine's once.Do closure re-acquiring
	// scopesMu, so a test can deterministically run a second attempt for the
	// same key inside that window instead of relying on goroutine scheduling
	// luck.
	blockFirstDelete chan struct{}
	// deleteStarted is closed by that first DeletePod call, so a test can
	// wait until it is definitely parked before proceeding, the same reason
	// waitStarted exists for blockFirstWait.
	deleteStarted chan struct{}
}

func (f *fakePM) CreatePod(_ context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	f.mu.Lock()
	f.created = pod
	f.createCount++
	out := pod.DeepCopy()
	out.Name = fmt.Sprintf("ucd-img-generated-%d", f.createCount) // simulate server-assigned name from GenerateName
	f.createdNm = out.Name
	f.createdNames = append(f.createdNames, out.Name)
	f.mu.Unlock()
	return out, nil
}
func (f *fakePM) WaitForPodRunning(ctx context.Context, _ string) error {
	deadline, hasDeadline := ctx.Deadline()
	f.mu.Lock()
	f.waitCtxSeen = true
	f.waitHadDeadline = hasDeadline
	f.waitDeadline = deadline
	waitErr := f.waitErr
	f.waitCount++
	callIndex := f.waitCount
	first := callIndex == 1
	gate := f.blockFirstWait
	started := f.waitStarted
	failLimit := f.failWaitCalls
	ignoreCtx := f.ignoreCtxUntilGate
	jitter := f.waitJitter
	f.mu.Unlock()

	if first {
		if started != nil {
			close(started)
		}
		if gate != nil {
			if ignoreCtx {
				<-gate
			} else {
				select {
				case <-gate:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
	if jitter > 0 {
		select {
		case <-time.After(time.Duration(rand.Int63n(int64(jitter)))):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if failLimit > 0 && callIndex > failLimit {
		return nil
	}
	return waitErr
}

// creations reports the number of pods CreatePod has produced so far.
func (f *fakePM) creations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCount
}
func (f *fakePM) DeletePod(ctx context.Context, name string) error {
	f.mu.Lock()
	f.deleteCount++
	first := f.deleteCount == 1
	gate := f.blockFirstDelete
	started := f.deleteStarted
	f.mu.Unlock()

	if first {
		if started != nil {
			close(started)
		}
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	f.mu.Lock()
	f.deleted = append(f.deleted, name)
	f.mu.Unlock()
	return nil
}
func (f *fakePM) ListPods(_ context.Context, _ string) (*corev1.PodList, error) {
	return &corev1.PodList{}, nil
}

type fakeExec struct {
	gotPod, gotContainer, gotScript string
	gotShell                        []string
	stdout                          string
	exit                            int
	err                             error
}

func (f *fakeExec) ExecStep(_ context.Context, podName, container, script string, shell []string, _ []string, stdout, _ io.Writer) (int, error) {
	f.gotPod, f.gotContainer, f.gotScript, f.gotShell = podName, container, script, shell
	_, _ = stdout.Write([]byte(f.stdout))
	return f.exit, f.err
}
func (f *fakeExec) ExecStepArgv(_ context.Context, podName, container string, argv []string, stdout, _ io.Writer) (int, error) {
	f.gotPod, f.gotContainer = podName, container
	if len(argv) > 0 {
		f.gotScript = strings.Join(argv, " ")
	}
	_, _ = stdout.Write([]byte(f.stdout))
	return f.exit, f.err
}
