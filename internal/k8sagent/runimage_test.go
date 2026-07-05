package k8sagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

type fakePM struct {
	created         *corev1.Pod
	createdNm       string
	waitErr         error
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
	stdout                          string
	exit                            int
	err                             error
}

func (f *fakeExec) ExecStep(_ context.Context, podName, container, script string, stdout, _ io.Writer) (int, error) {
	f.gotPod, f.gotContainer, f.gotScript = podName, container, script
	_, _ = stdout.Write([]byte(f.stdout))
	return f.exit, f.err
}
func (f *fakeExec) ExecStepArgv(context.Context, string, string, []string, io.Writer, io.Writer) (int, error) {
	return 0, nil
}

func TestRunImageStep_CreatesExecsDeletes(t *testing.T) {
	pm := &fakePM{}
	ex := &fakeExec{stdout: "hi\n", exit: 0}
	a := &K8sAgent{pm: pm, exec: ex}

	var out, errBuf bytes.Buffer
	code, err := a.runImageStep(context.Background(), "run-1", "alpine:3.20",
		map[string]string{"FOO": "bar"}, 1800, "echo hi", &out, &errBuf)

	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, "hi\n", out.String())
	// created the right pod
	require.NotNil(t, pm.created)
	assert.Equal(t, "alpine:3.20", pm.created.Spec.Containers[0].Image)
	// exec targeted the created pod + "step" container + script
	assert.Equal(t, "ucd-img-generated-xyz", ex.gotPod)
	assert.Equal(t, "step", ex.gotContainer)
	assert.Equal(t, "echo hi", ex.gotScript)
	// pod deleted exactly once
	assert.Equal(t, []string{"ucd-img-generated-xyz"}, pm.deleted)
	// wait was bounded by a deadline (imagePodStartTimeout), not the bare run ctx
	assert.True(t, pm.waitHadDeadline, "WaitForPodRunning should be called with a deadline-bounded context")
}

func TestRunImageStep_WaitBoundedByTimeout(t *testing.T) {
	pm := &fakePM{}
	ex := &fakeExec{stdout: "ok\n", exit: 0}
	a := &K8sAgent{pm: pm, exec: ex}

	code, err := a.runImageStep(context.Background(), "run-1", "alpine:3.20",
		nil, 3600, "true", io.Discard, io.Discard)

	require.NoError(t, err)
	assert.Equal(t, 0, code)
	require.True(t, pm.waitCtxSeen)
	assert.True(t, pm.waitHadDeadline, "runImageStep must bound WaitForPodRunning with a timeout so a stuck pod fails fast")
}

func TestRunImageStep_DeletesOnWaitFailure(t *testing.T) {
	pm := &fakePM{waitErr: errors.New("ImagePullBackOff")}
	ex := &fakeExec{}
	a := &K8sAgent{pm: pm, exec: ex}

	code, err := a.runImageStep(context.Background(), "run-1", "no/such:img",
		nil, 3600, "true", io.Discard, io.Discard)

	require.Error(t, err)
	assert.Equal(t, -1, code)
	assert.Empty(t, ex.gotPod, "exec must not run when the pod never becomes ready")
	// cleanup still happened despite the failure
	assert.Equal(t, []string{"ucd-img-generated-xyz"}, pm.deleted)
}
