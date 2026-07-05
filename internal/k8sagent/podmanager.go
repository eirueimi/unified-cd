package k8sagent

import (
	"context"
	"fmt"
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
func (pm *PodManager) WaitForPodRunning(ctx context.Context, podName string) error {
	for {
		pod, err := pm.client.CoreV1().Pods(pm.namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get Pod %s: %w", podName, err)
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			return nil
		case corev1.PodFailed, corev1.PodSucceeded:
			return fmt.Errorf("Pod %s entered unexpected phase %s", podName, pod.Status.Phase)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			time.Sleep(500 * time.Millisecond)
		}
	}
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
