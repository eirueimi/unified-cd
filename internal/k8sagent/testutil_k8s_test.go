//go:build k8s

package k8sagent

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var runSeq atomic.Int64

const testImage = "ubuntu:22.04"

// newTestKubeClient returns a kubernetes client and rest config for integration tests.
// Skips the test if no cluster is reachable via the default kubeconfig.
func newTestKubeClient(t *testing.T) (*kubernetes.Clientset, *rest.Config) {
	t.Helper()
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		t.Skipf("no kubernetes cluster available: %v", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("kubernetes client: %v", err)
	}
	if _, err := client.Discovery().ServerVersion(); err != nil {
		t.Skipf("kubernetes cluster not reachable: %v", err)
	}
	return client, cfg
}

// newTestNamespace creates a unique Kubernetes namespace for a test and deletes it on cleanup.
// The namespace name is derived from t.Name(), sanitized to be a valid DNS label.
func newTestNamespace(t *testing.T, client *kubernetes.Clientset) string {
	t.Helper()
	raw := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		case r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, t.Name())
	if len(raw) > 50 {
		raw = raw[:50]
	}
	name := "test-" + strings.Trim(raw, "-")
	_, err := client.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create test namespace %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = client.CoreV1().Namespaces().Delete(context.Background(), name, metav1.DeleteOptions{})
	})
	return name
}

// podReadyOrSkip creates a Pod with the given runID and waits for it to reach Running state.
// Returns the created pod name. Registers cleanup to delete the pod on test completion.
func podReadyOrSkip(t *testing.T, pm *PodManager, runID string) string {
	t.Helper()
	ctx := context.Background()
	// See podmanager_integration_test.go's note: testImage stands in for a
	// real shim image here purely to compile; not ucd-sh-capable.
	pod, err := BuildPod(runID, pm.namespace, nil, nil, testImage, SidecarSpec{}, testImage)
	if err != nil {
		t.Fatalf("BuildPod: %v", err)
	}
	created, err := pm.CreatePod(ctx, pod)
	if err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	t.Cleanup(func() { _ = pm.DeletePod(context.Background(), created.Name) })
	if err := pm.WaitForPodRunning(ctx, created.Name); err != nil {
		t.Fatalf("WaitForPodRunning: %v", err)
	}
	return created.Name
}

// uniqueRunID returns a unique test-scoped run ID using a package-level atomic counter.
func uniqueRunID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, runSeq.Add(1))
}
