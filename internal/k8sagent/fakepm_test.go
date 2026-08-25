package k8sagent

import (
	"context"
	"fmt"
	"io"
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
	waitErr   error
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
}

func (f *fakePM) CreatePod(_ context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	f.mu.Lock()
	f.created = pod
	f.createCount++
	out := pod.DeepCopy()
	out.Name = fmt.Sprintf("ucd-img-generated-%d", f.createCount) // simulate server-assigned name from GenerateName
	f.createdNm = out.Name
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
	first := f.waitCount == 1
	gate := f.blockFirstWait
	started := f.waitStarted
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
	return waitErr
}

// creations reports the number of pods CreatePod has produced so far.
func (f *fakePM) creations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCount
}
func (f *fakePM) DeletePod(_ context.Context, name string) error {
	f.deleted = append(f.deleted, name)
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
