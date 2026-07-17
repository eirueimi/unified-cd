package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestKubernetesEnrollmentVerifier_VerifiesBoundPod(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-0", Namespace: "unified-cd", UID: "pod-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "unified-cd-k8s-agent"}})
	client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, []string{KubernetesEnrollmentAudience}))
	verifier := NewKubernetesEnrollmentVerifier("prod", client)
	identity, err := verifier.Verify(t.Context(), projectedToken("unified-cd", "unified-cd-k8s-agent", "agent-0", "pod-uid"), kubernetesEnrollmentPolicy())
	require.NoError(t, err)
	assert.Equal(t, KubernetesEnrollmentIdentity{Cluster: "prod", Namespace: "unified-cd", ServiceAccount: "unified-cd-k8s-agent", PodName: "agent-0", PodUID: "pod-uid"}, identity)
}

func TestKubernetesEnrollmentVerifier_FailsClosedForInvalidBindings(t *testing.T) {
	basePolicy := kubernetesEnrollmentPolicy()
	cases := []struct {
		name      string
		token     string
		policy    store.AgentEnrollmentPolicy
		pod       *corev1.Pod
		audiences []string
	}{
		{"wrong audience", projectedToken("unified-cd", "unified-cd-k8s-agent", "agent-0", "pod-uid"), basePolicy, boundPod(), []string{"other"}},
		{"namespace policy mismatch", projectedToken("other", "unified-cd-k8s-agent", "agent-0", "pod-uid"), basePolicy, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-0", Namespace: "other", UID: "pod-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "unified-cd-k8s-agent"}}, []string{KubernetesEnrollmentAudience}},
		{"service account policy mismatch", projectedToken("unified-cd", "other", "agent-0", "pod-uid"), basePolicy, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-0", Namespace: "unified-cd", UID: "pod-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "other"}}, []string{KubernetesEnrollmentAudience}},
		{"pod UID mismatch", projectedToken("unified-cd", "unified-cd-k8s-agent", "agent-0", "different"), basePolicy, boundPod(), []string{KubernetesEnrollmentAudience}},
		{"deleted pod", projectedToken("unified-cd", "unified-cd-k8s-agent", "agent-0", "pod-uid"), basePolicy, nil, []string{KubernetesEnrollmentAudience}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objects := []runtime.Object{}
			if tc.pod != nil {
				objects = append(objects, tc.pod)
			}
			client := fake.NewSimpleClientset(objects...)
			client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, tc.audiences))
			_, err := NewKubernetesEnrollmentVerifier("prod", client).Verify(context.Background(), tc.token, tc.policy)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrKubernetesEnrollmentRejected)
		})
	}
}

func TestKubernetesEnrollment_AuthorizationRejectsRequestedLabelEscalation(t *testing.T) {
	policy := kubernetesEnrollmentPolicy()
	policy.AllowedLabels = []string{"kind:kubernetes"}
	policy.RequiredLabels = []string{"kind:kubernetes"}
	policy.AuthorizedCapabilities = []string{"pod"}
	_, _, ok := authorizedKubernetesRequest(api.AgentEnrollRequest{Labels: []string{"pool:admin"}}, policy)
	assert.False(t, ok)
	_, _, ok = authorizedKubernetesRequest(api.AgentEnrollRequest{Capabilities: []string{"native"}}, policy)
	assert.False(t, ok)
}

func kubernetesEnrollmentPolicy() store.AgentEnrollmentPolicy {
	return store.AgentEnrollmentPolicy{Provider: "kubernetes", SubjectConstraints: json.RawMessage(`{"namespaces":["unified-cd"],"serviceAccounts":["unified-cd-k8s-agent"]}`)}
}
func boundPod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-0", Namespace: "unified-cd", UID: "pod-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "unified-cd-k8s-agent"}}
}
func tokenReviewReactor(authenticated bool, audiences []string) k8stesting.ReactionFunc {
	return func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: authenticated, Audiences: audiences}}, nil
	}
}
func projectedToken(namespace, serviceAccount, podName, podUID string) string {
	payload, _ := json.Marshal(map[string]any{"kubernetes.io": map[string]any{"namespace": namespace, "serviceaccount": map[string]string{"name": serviceAccount}, "pod": map[string]string{"name": podName, "uid": podUID}}})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
