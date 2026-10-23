package k8sagent

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// podManager grew GetPod (explainPodLoss). These fakes never serve a pod, so a
// lookup is NotFound, which callers treat as "keep the original error".
func notFoundPod(name string) (*corev1.Pod, error) {
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
}

func (envCapturePM) GetPod(_ context.Context, n string) (*corev1.Pod, error) { return notFoundPod(n) }
func (f *scopeLifecyclePM) GetPod(_ context.Context, n string) (*corev1.Pod, error) {
	return notFoundPod(n)
}
func (f *listRunPodsPM) GetPod(_ context.Context, n string) (*corev1.Pod, error) {
	return notFoundPod(n)
}
func (f *secretsPM) GetPod(_ context.Context, n string) (*corev1.Pod, error) { return notFoundPod(n) }
