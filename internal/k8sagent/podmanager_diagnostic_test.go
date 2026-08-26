package k8sagent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// waitingPod builds a Pending Pod whose named container is waiting with the
// given reason/message, the shape the kubelet reports for a container it could
// not configure or pull.
func waitingPod(name, container, reason, message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ci"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  container,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
			}},
		},
	}
}

// TestWaitForPodRunning_SurfacesWaitingReason is the regression test for the
// operator's second failure. `sidecarS3SecretName` named a Secret that existed
// in the AGENT's namespace but not in the job Pod's, so the kubelet failed the
// unified-artifact container with CreateContainerConfigError (envFrom.secretRef
// carries no `optional: true`). The wait polled phase only, so the operator got
// five minutes of silence followed by a bare "context deadline exceeded" — the
// one string that would have explained everything, `secret "..." not found`,
// never reached them.
func TestWaitForPodRunning_SurfacesWaitingReason(t *testing.T) {
	pod := waitingPod("ucd-run-1", artifactSidecarName,
		"CreateContainerConfigError", `secret "unified-cd-s3-creds" not found`)
	pm := NewPodManager(fake.NewSimpleClientset(pod), "ci", "img")

	// A short deadline stands in for podStartTimeout expiring.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := pm.WaitForPodRunning(ctx, "ucd-run-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CreateContainerConfigError", "the kubelet's waiting reason must reach the returned error")
	assert.Contains(t, err.Error(), `secret "unified-cd-s3-creds" not found`, "the waiting MESSAGE is the part that actually names the problem")
	assert.Contains(t, err.Error(), artifactSidecarName, "the error must say which container is stuck")
	assert.ErrorIs(t, err, context.DeadlineExceeded, "the timeout must still be identifiable by callers")
}

// TestWaitForPodRunning_ImagePullBackOffIsDiagnosedNotShortCircuited covers the
// other reason the troubleshooting page currently sends operators to `kubectl
// describe`: the reason now reaches the error, but the wait is NOT cut short.
// ImagePullBackOff recovers on its own the moment the image becomes pullable,
// so failing early would turn a self-healing condition into a hard failure.
func TestWaitForPodRunning_ImagePullBackOffIsDiagnosedNotShortCircuited(t *testing.T) {
	pod := waitingPod("ucd-run-2", artifactSidecarName, "ImagePullBackOff", `Back-off pulling image "sidecar:nope"`)
	pm := NewPodManager(fake.NewSimpleClientset(pod), "ci", "img")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := pm.WaitForPodRunning(ctx, "ucd-run-2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ImagePullBackOff")
	assert.ErrorIs(t, err, context.DeadlineExceeded, "a recoverable reason must ride out the timeout, not fail early")
	assert.GreaterOrEqual(t, time.Since(start), 50*time.Millisecond)
}

// TestWaitForPodRunning_TerminalReasonShortCircuits: InvalidImageName is the
// one reason nothing external can fix — the image reference does not parse, and
// no Secret/ConfigMap appearing or registry recovering changes that — so the
// wait fails immediately instead of burning the full podStartTimeout.
func TestWaitForPodRunning_TerminalReasonShortCircuits(t *testing.T) {
	pod := waitingPod("ucd-run-3", primaryContainerName, "InvalidImageName", `couldn't parse image name "::::"`)
	pm := NewPodManager(fake.NewSimpleClientset(pod), "ci", "img")

	// A generous deadline: if the wait did NOT short-circuit, this test would
	// block for the whole 10s rather than returning promptly.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := pm.WaitForPodRunning(ctx, "ucd-run-3")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidImageName")
	assert.NotErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second, "a terminal waiting reason must not burn the full podStartTimeout")
}

// TestPodStartDiagnostic covers the summarizer directly: init containers, an
// unschedulable Pod with no container statuses at all, and the benign cases
// that must stay quiet.
func TestPodStartDiagnostic(t *testing.T) {
	t.Run("init container waiting", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  ucdShimContainerName,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "no such host"}},
			}},
		}}
		detail, terminal, severe := podStartDiagnostic(pod)
		assert.Contains(t, detail, "init container")
		assert.Contains(t, detail, ucdShimContainerName)
		assert.Contains(t, detail, "ErrImagePull: no such host")
		assert.False(t, terminal)
		assert.True(t, severe, "a failed image pull is worth a human's attention")
	})

	t.Run("unschedulable Pod has no container statuses", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type:    corev1.PodScheduled,
				Status:  corev1.ConditionFalse,
				Reason:  "Unschedulable",
				Message: "0/3 nodes are available: insufficient cpu",
			}},
		}}
		detail, terminal, severe := podStartDiagnostic(pod)
		assert.Contains(t, detail, "Unschedulable")
		assert.Contains(t, detail, "insufficient cpu")
		assert.False(t, terminal)
		assert.True(t, severe, "the scheduler actively rejected this Pod")
	})

	t.Run("running Pod offers nothing to report", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  primaryContainerName,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		}}
		detail, terminal, severe := podStartDiagnostic(pod)
		assert.Empty(t, detail)
		assert.False(t, terminal)
		assert.False(t, severe)
	})

	// The benign cases are the whole reason severity exists: EVERY healthy Pod
	// passes through them, so if they counted as severe, every single
	// Kubernetes run would emit a WARN and operators would learn to skim past
	// the line that actually matters.
	t.Run("normal startup is not severe", func(t *testing.T) {
		for _, reason := range []string{"ContainerCreating", "PodInitializing"} {
			t.Run(reason, func(t *testing.T) {
				pod := waitingPod("p", primaryContainerName, reason, "")
				detail, terminal, severe := podStartDiagnostic(pod)
				assert.Contains(t, detail, reason, "the reason still belongs in the returned error")
				assert.False(t, terminal)
				assert.False(t, severe, "a normal startup transition must not raise the log level")
			})
		}
	})

	t.Run("every stuck container is reported, not just the first", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: primaryContainerName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}},
				{Name: artifactSidecarName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: `secret "s3" not found`}}},
			},
		}}
		detail, _, severe := podStartDiagnostic(pod)
		assert.Contains(t, detail, "ContainerCreating")
		assert.Contains(t, detail, `secret "s3" not found`)
		assert.True(t, severe, "one benign container must not mask a severe sibling")
	})
}
