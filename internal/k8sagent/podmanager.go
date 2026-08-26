package k8sagent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PodManager creates and manages Kubernetes Pods for job execution.
type PodManager struct {
	client    kubernetes.Interface
	namespace string
	podImage  string
}

// NewPodManager creates a new PodManager.
func NewPodManager(client kubernetes.Interface, namespace, podImage string) *PodManager {
	return &PodManager{client: client, namespace: namespace, podImage: podImage}
}

// Client returns the underlying Kubernetes client, for callers (e.g. the
// sidecar log pump) that need direct API access beyond PodManager's own
// pod-lifecycle methods.
func (pm *PodManager) Client() kubernetes.Interface { return pm.client }

// CreateJobPod creates a Pod corresponding to the given runID.
func (pm *PodManager) CreateJobPod(ctx context.Context, runID string, labels map[string]string) (*corev1.Pod, error) {
	pod := pm.buildPodSpec(runID)
	if labels != nil {
		for k, v := range labels {
			pod.Labels[k] = v
		}
	}
	created, err := pm.client.CoreV1().Pods(pm.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create Pod for run %s: %w", runID, err)
	}
	return created, nil
}

// DeletePod deletes the specified Pod.
func (pm *PodManager) DeletePod(ctx context.Context, podName string) error {
	policy := metav1.DeletePropagationForeground
	err := pm.client.CoreV1().Pods(pm.namespace).Delete(ctx, podName, metav1.DeleteOptions{
		PropagationPolicy: &policy,
	})
	if err != nil {
		return fmt.Errorf("failed to delete Pod %s: %w", podName, err)
	}
	return nil
}

// CreatePod creates a pre-built Pod object in Kubernetes.
func (pm *PodManager) CreatePod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	pod.Namespace = pm.namespace
	created, err := pm.client.CoreV1().Pods(pm.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create Pod %s: %w", pod.Name, err)
	}
	return created, nil
}

// ListPods lists Pods in the manager's namespace matching the given label
// selector (e.g. "app=unified-cd-agent"). Used by the orphan-pod GC sweep.
func (pm *PodManager) ListPods(ctx context.Context, labelSelector string) (*corev1.PodList, error) {
	return pm.client.CoreV1().Pods(pm.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
}

// UpdatePodAnnotations updates Pod annotations using optimistic concurrency control.
// Returns a conflict error if the resourceVersion does not match.
func (pm *PodManager) UpdatePodAnnotations(ctx context.Context, podName string, annotations map[string]string, resourceVersion string) error {
	pod, err := pm.client.CoreV1().Pods(pm.namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get Pod %s: %w", podName, err)
	}
	if pod.ResourceVersion != resourceVersion {
		return fmt.Errorf("conflict: resourceVersion of pod %s has changed", podName)
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		pod.Annotations[k] = v
	}
	_, err = pm.client.CoreV1().Pods(pm.namespace).Update(ctx, pod, metav1.UpdateOptions{})
	return err
}

// WaitForPodRunning polls and waits until the Pod reaches Running state.
//
// Every error it returns carries the kubelet's own explanation of why the Pod
// is not Running — the container waiting reason/message, or the scheduler's
// rejection — so an operator reads `secret "unified-cd-s3-creds" not found`
// (the classic sidecarS3SecretName-points-at-a-Secret-in-the-wrong-namespace
// case, where envFrom.secretRef has no optional: true and the kubelet fails
// the container with CreateContainerConfigError) or `ImagePullBackOff` from
// the run's own failure message, instead of a bare "context deadline exceeded"
// that sends them to `kubectl describe pod`. The same detail is logged as soon
// as it appears, so it is visible immediately rather than only once
// podStartTimeout expires.
func (pm *PodManager) WaitForPodRunning(ctx context.Context, podName string) error {
	var lastDetail string
	for {
		pod, err := pm.client.CoreV1().Pods(pm.namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get Pod %s: %w", podName, err)
		}
		detail, terminal, severe := podStartDiagnostic(pod)
		if detail != "" && detail != lastDetail {
			// Severity matters more than it looks. Every normal Pod start passes
			// through ContainerCreating/PodInitializing, so logging the whole
			// diagnostic at Warn would put at least one WARN on EVERY Kubernetes
			// run — training an operator to skim past the one line this branch
			// exists to make them read. Benign transitions go to Info; Warn is
			// reserved for the pull and config failures that need a human.
			if severe {
				slog.Warn("k8s: Pod is not Running yet", "pod", podName, "namespace", pm.namespace, "detail", detail)
			} else {
				slog.Info("k8s: waiting for Pod to start", "pod", podName, "namespace", pm.namespace, "detail", detail)
			}
			lastDetail = detail
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			return nil
		case corev1.PodFailed, corev1.PodSucceeded:
			return fmt.Errorf("Pod %s entered unexpected phase %s%s", podName, pod.Status.Phase, detailSuffix(detail))
		}
		if terminal {
			return fmt.Errorf("Pod %s cannot start: %s", podName, detail)
		}
		select {
		case <-ctx.Done():
			// detail, not lastDetail: report what is true NOW. A diagnostic that
			// has since cleared is not the reason this wait timed out, and naming
			// it would send the operator after a problem that already resolved.
			return fmt.Errorf("waiting for Pod %s to become Running: %w%s", podName, ctx.Err(), detailSuffix(detail))
		default:
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// terminalWaitingReasons are container waiting reasons the kubelet can never
// clear by itself, so continuing to poll until podStartTimeout only delays a
// certain failure.
//
// The set is deliberately tiny. Most waiting reasons that LOOK terminal are
// not: `ImagePullBackOff`/`ErrImagePull` recover the moment the image becomes
// pullable (registry blip, imagePullSecret added, tag pushed late), and
// `CreateContainerConfigError` — the reason behind the missing-Secret failure
// this diagnostic exists for — recovers as soon as the referenced Secret or
// ConfigMap exists, which is exactly what happens when a GitOps apply lands
// the Secret moments after the agent claimed the run. Short-circuiting those
// would turn a self-healing race into a hard failure, so they are only
// reported, never acted on.
//
// `InvalidImageName` is different in kind: the image reference itself is
// malformed, so no external object appearing can make it parse. Nothing in
// this agent mutates a Pod's images after creation, so the kubelet will retry
// the same unparseable string until the timeout with a guaranteed outcome.
var terminalWaitingReasons = map[string]bool{
	"InvalidImageName": true,
}

// benignWaitingReasons are the waiting reasons every healthy Pod passes
// through on its way to Running. They belong in a returned error (by then
// something has already gone wrong, and knowing the Pod never got past
// ContainerCreating is the useful part) but must not raise the log level while
// the Pod is simply still starting.
var benignWaitingReasons = map[string]bool{
	"ContainerCreating": true,
	"PodInitializing":   true,
}

// podStartDiagnostic summarizes why a not-yet-Running Pod has not started,
// from the kubelet's container statuses (init containers first — the ucd-shim
// init container blocks every other container behind it) and, when no
// container has reported at all, from an unschedulable PodScheduled condition.
//
// terminal reports whether any waiting reason is in terminalWaitingReasons.
// severe reports whether anything seen is worth a human's attention now, i.e.
// any reason outside benignWaitingReasons — it governs log level only, never
// what the returned error says.
func podStartDiagnostic(pod *corev1.Pod) (detail string, terminal, severe bool) {
	var parts []string
	collect := func(kind string, statuses []corev1.ContainerStatus) {
		for _, cs := range statuses {
			w := cs.State.Waiting
			if w == nil || w.Reason == "" {
				continue
			}
			part := fmt.Sprintf("%s %q is waiting: %s", kind, cs.Name, w.Reason)
			if w.Message != "" {
				part += ": " + w.Message
			}
			parts = append(parts, part)
			if terminalWaitingReasons[w.Reason] {
				terminal = true
			}
			if !benignWaitingReasons[w.Reason] {
				severe = true
			}
		}
	}
	collect("init container", pod.Status.InitContainerStatuses)
	collect("container", pod.Status.ContainerStatuses)
	if len(parts) == 0 {
		// No container has been created yet: the Pod is most likely unschedulable
		// (no node fits its resources/affinity), which the scheduler records as a
		// PodScheduled=False condition rather than a container waiting reason.
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason != "" {
				part := "Pod is not scheduled: " + cond.Reason
				if cond.Message != "" {
					part += ": " + cond.Message
				}
				parts = append(parts, part)
				// The scheduler actively rejected this Pod (Unschedulable,
				// SchedulerError); unlike ContainerCreating that is not a phase
				// every healthy Pod passes through.
				severe = true
			}
		}
	}
	return strings.Join(parts, "; "), terminal, severe
}

// detailSuffix renders a podStartDiagnostic detail as a trailing clause, or
// nothing at all when the kubelet offered no explanation.
func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return " (" + detail + ")"
}

// buildPodSpec constructs a Pod object corresponding to the given runID.
func (pm *PodManager) buildPodSpec(runID string) *corev1.Pod {
	// Pod name must be DNS-compliant, so truncate the runID
	suffix := runID
	if len(suffix) > 16 {
		suffix = suffix[:16]
	}
	podName := fmt.Sprintf("excd-run-%s", suffix)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: pm.namespace,
			Labels: map[string]string{
				"app":                "unified-cd-agent",
				"unified-cd/runId": runID,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:    "job",
					Image:   pm.podImage,
					Command: []string{"sleep", "infinity"},
				},
			},
		},
	}
}
