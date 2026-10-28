//go:build k8s

package k8sagent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// TestAgentBuiltPodsAreAdmittedUnderBaseline proves spec §6's claim against a
// real Pod Security admission controller: every Pod shape the agent builds is
// admitted in a namespace that enforces baseline. Server-side dry-run runs
// admission without scheduling anything, so no image is pulled.
func TestAgentBuiltPodsAreAdmittedUnderBaseline(t *testing.T) {
	client, _ := newTestKubeClient(t)
	ns := newTestNamespace(t, client)
	ctx := context.Background()
	nsObj, err := client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	require.NoError(t, err)
	nsObj.Labels = map[string]string{"pod-security.kubernetes.io/enforce": "baseline"}
	_, err = client.CoreV1().Namespaces().Update(ctx, nsObj, metav1.UpdateOptions{})
	require.NoError(t, err)
	dry := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}

	for _, mode := range []string{SidecarS3SecretModeEnv, SidecarS3SecretModeFile, SidecarS3SecretModeBroker} {
		sc := SidecarSpec{Image: "sidecar:test", S3SecretName: "ucd-s3", S3SecretMode: mode, BrokerURL: "https://c.example"}
		vols := []PodVolume{{Name: "go-cache", ClaimName: "ci-main-go-cache", MountPath: "cache"}}
		pod, err := BuildPod(uniqueRunID("baseline-"+mode), ns, nil, nil, testImage, sc, testShimImage, vols)
		require.NoError(t, err)
		_, err = client.CoreV1().Pods(ns).Create(ctx, pod, dry)
		require.NoError(t, err, "agent-built run Pod (sidecar mode %s) must be admitted under baseline", mode)

		scope := buildScopePod(uniqueRunID("scope-"+mode), ns, "scope:x", testImage, map[string]string{"A": "1"}, sc, testShimImage, corev1.ResourceRequirements{})
		_, err = client.CoreV1().Pods(ns).Create(ctx, scope, dry)
		require.NoError(t, err, "agent-built scope Pod (sidecar mode %s) must be admitted under baseline", mode)
	}

	// Negative control: admission is really enforcing, so the passes above mean something.
	bad, err := BuildPod(uniqueRunID("baseline-bad"), ns, nil, nil, testImage, SidecarSpec{}, testShimImage, nil)
	require.NoError(t, err)
	bad.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
	_, err = client.CoreV1().Pods(ns).Create(ctx, bad, dry)
	require.Error(t, err)
	require.Contains(t, err.Error(), "violates PodSecurity")
}
