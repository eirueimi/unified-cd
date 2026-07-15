package k8sagent

import (
	"context"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// fakePM and fakeExec are the shared k8sagent test fixtures (a fake podManager
// and a fake step executor) used across the backend/scope/orchestrate suites.
// They record the pod they created and the exec call they received so tests can
// assert on the pod spec and exec target.
type fakePM struct {
	created         *corev1.Pod
	createdNm       string
	waitErr         error
	waitBlock       chan struct{} // if non-nil, WaitForPodRunning blocks until closed or ctx done
	deleted         []string
	waitHadDeadline bool
	waitCtxSeen     bool
}

func (f *fakePM) CreatePod(_ context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	f.created = pod
	out := pod.DeepCopy()
	out.Name = "ucd-img-generated-xyz" // simulate server-assigned name from GenerateName
	f.createdNm = out.Name
	return out, nil
}
func (f *fakePM) WaitForPodRunning(ctx context.Context, _ string) error {
	f.waitCtxSeen = true
	_, hasDeadline := ctx.Deadline()
	f.waitHadDeadline = hasDeadline
	if f.waitBlock != nil {
		select {
		case <-f.waitBlock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.waitErr
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
