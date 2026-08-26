package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, []string{KubernetesEnrollmentAudience}, "system:serviceaccount:unified-cd:unified-cd-k8s-agent", "sa-uid"))
	verifier := NewKubernetesEnrollmentVerifier("prod", client)
	identity, err := verifier.Verify(t.Context(), projectedToken("unified-cd", "unified-cd-k8s-agent", "sa-uid", "agent-0", "pod-uid"), kubernetesEnrollmentPolicy())
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
		{"wrong audience", projectedToken("unified-cd", "unified-cd-k8s-agent", "sa-uid", "agent-0", "pod-uid"), basePolicy, boundPod(), []string{"other"}},
		{"namespace policy mismatch", projectedToken("other", "unified-cd-k8s-agent", "sa-uid", "agent-0", "pod-uid"), basePolicy, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-0", Namespace: "other", UID: "pod-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "unified-cd-k8s-agent"}}, []string{KubernetesEnrollmentAudience}},
		{"service account policy mismatch", projectedToken("unified-cd", "other", "sa-uid", "agent-0", "pod-uid"), basePolicy, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-0", Namespace: "unified-cd", UID: "pod-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "other"}}, []string{KubernetesEnrollmentAudience}},
		{"pod UID mismatch", projectedToken("unified-cd", "unified-cd-k8s-agent", "sa-uid", "agent-0", "different"), basePolicy, boundPod(), []string{KubernetesEnrollmentAudience}},
		{"deleted pod", projectedToken("unified-cd", "unified-cd-k8s-agent", "sa-uid", "agent-0", "pod-uid"), basePolicy, nil, []string{KubernetesEnrollmentAudience}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objects := []runtime.Object{}
			if tc.pod != nil {
				objects = append(objects, tc.pod)
			}
			client := fake.NewSimpleClientset(objects...)
			client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, tc.audiences, "system:serviceaccount:unified-cd:unified-cd-k8s-agent", "sa-uid"))
			_, err := NewKubernetesEnrollmentVerifier("prod", client).Verify(context.Background(), tc.token, tc.policy)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrKubernetesEnrollmentRejected)
		})
	}
}

func TestKubernetesEnrollmentVerifier_RejectsAuthenticatedNonServiceAccountAndUIDMismatch(t *testing.T) {
	for _, tc := range []struct{ name, username, tokenUID, reviewUID string }{
		{"authenticated OIDC token", "oidc:subject", "sa-uid", ""},
		{"service account UID mismatch", "system:serviceaccount:unified-cd:unified-cd-k8s-agent", "sa-uid", "other-uid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(boundPod())
			client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, []string{KubernetesEnrollmentAudience}, tc.username, tc.reviewUID))
			_, err := NewKubernetesEnrollmentVerifier("prod", client).Verify(t.Context(), projectedToken("unified-cd", "unified-cd-k8s-agent", tc.tokenUID, "agent-0", "pod-uid"), kubernetesEnrollmentPolicy())
			require.ErrorIs(t, err, ErrKubernetesEnrollmentRejected)
		})
	}
}

// TestKubernetesEnrollmentVerifier_AcceptsLiveApiServerTokenReviewShape pins the
// verifier to the response a real API server sends, independently of the shared
// reactor helper. It is the regression guard for the defect where the verifier
// read the ServiceAccount UID from a
// "authentication.kubernetes.io/serviceaccount.uid" extra that Kubernetes never
// populates, so every Kubernetes enrollment was rejected 403 unconditionally
// while the unit suite stayed green.
func TestKubernetesEnrollmentVerifier_AcceptsLiveApiServerTokenReviewShape(t *testing.T) {
	client := fake.NewSimpleClientset(boundPod())
	client.Fake.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{
			Authenticated: true,
			Audiences:     []string{KubernetesEnrollmentAudience},
			User: authv1.UserInfo{
				Username: "system:serviceaccount:unified-cd:unified-cd-k8s-agent",
				UID:      "sa-uid",
				Extra:    serviceAccountTokenExtras(),
			},
		}}, nil
	})

	identity, err := NewKubernetesEnrollmentVerifier("prod", client).Verify(t.Context(), projectedToken("unified-cd", "unified-cd-k8s-agent", "sa-uid", "agent-0", "pod-uid"), kubernetesEnrollmentPolicy())
	require.NoError(t, err, "a TokenReview shaped like a real API server response must enroll")
	assert.Equal(t, KubernetesEnrollmentIdentity{Cluster: "prod", Namespace: "unified-cd", ServiceAccount: "unified-cd-k8s-agent", PodName: "agent-0", PodUID: "pod-uid"}, identity)
}

func TestKubernetesEnrollmentVerifier_RejectsMissingOrMismatchedServiceAccountUID(t *testing.T) {
	for _, tc := range []struct {
		name        string
		reviewedUID string
		extra       map[string]authv1.ExtraValue
	}{
		{"missing service account UID", "", serviceAccountTokenExtras()},
		{"mismatched service account UID", "other-uid", serviceAccountTokenExtras()},
		// The verifier must not fall back to the extras map: a caller able to
		// influence extras but not the reviewed subject UID must not enroll.
		{"UID only in a serviceaccount.uid extra", "", map[string]authv1.ExtraValue{"authentication.kubernetes.io/serviceaccount.uid": {"sa-uid"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(boundPod())
			client.Fake.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{
					Authenticated: true,
					Audiences:     []string{KubernetesEnrollmentAudience},
					User: authv1.UserInfo{
						Username: "system:serviceaccount:unified-cd:unified-cd-k8s-agent",
						UID:      tc.reviewedUID,
						Extra:    tc.extra,
					},
				}}, nil
			})

			_, err := NewKubernetesEnrollmentVerifier("prod", client).Verify(t.Context(), projectedToken("unified-cd", "unified-cd-k8s-agent", "sa-uid", "agent-0", "pod-uid"), kubernetesEnrollmentPolicy())
			require.ErrorIs(t, err, ErrKubernetesEnrollmentRejected)
		})
	}
}

func TestKubernetesEnrollmentVerifier_MapsTokenReviewDeadlineToUnavailable(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.Fake.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, context.DeadlineExceeded })
	_, err := NewKubernetesEnrollmentVerifier("prod", client).Verify(t.Context(), projectedToken("unified-cd", "unified-cd-k8s-agent", "sa-uid", "agent-0", "pod-uid"), kubernetesEnrollmentPolicy())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrKubernetesEnrollmentUnavailable))
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestKubernetesEnrollmentVerifier_PreservesPodLookupDeadline(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, []string{KubernetesEnrollmentAudience}, "system:serviceaccount:unified-cd:unified-cd-k8s-agent", "sa-uid"))
	client.Fake.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, context.DeadlineExceeded })

	_, err := NewKubernetesEnrollmentVerifier("prod", client).Verify(t.Context(), projectedToken("unified-cd", "unified-cd-k8s-agent", "sa-uid", "agent-0", "pod-uid"), kubernetesEnrollmentPolicy())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrKubernetesEnrollmentUnavailable)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestKubernetesEnrollment_HandlerMapsVerifierDeadlineToRetryableUnavailable(t *testing.T) {
	policy := kubernetesEnrollmentPolicy()
	policy.Name = "prod-policy"
	policy.Enabled = true
	policy.ProviderConfig = json.RawMessage(`{"cluster":"prod"}`)
	s := NewServer(Config{KubernetesEnrollmentVerifiers: map[string]KubernetesEnrollmentVerifier{"prod": unavailableKubernetesVerifier{}}}, unavailableKubernetesPolicyStore{policy: policy})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/enroll", bytes.NewBufferString(`{"provider":"kubernetes","policy":"prod-policy"}`))
	req.Header.Set("Authorization", "Bearer projected-token")
	rec := httptest.NewRecorder()
	s.handleAgentEnroll(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "kubernetes identity unavailable\n", rec.Body.String())
}

type unavailableKubernetesVerifier struct{}

func (unavailableKubernetesVerifier) Verify(context.Context, string, store.AgentEnrollmentPolicy) (KubernetesEnrollmentIdentity, error) {
	return KubernetesEnrollmentIdentity{}, ErrKubernetesEnrollmentUnavailable
}

type unavailableKubernetesPolicyStore struct {
	store.Store
	policy store.AgentEnrollmentPolicy
}

func (s unavailableKubernetesPolicyStore) GetAgentEnrollmentPolicy(context.Context, string) (*store.AgentEnrollmentPolicy, error) {
	return &s.policy, nil
}

func TestKubernetesEnrollment_AuthorizationRejectsRequestedLabelEscalation(t *testing.T) {
	policy := kubernetesEnrollmentPolicy()
	policy.AllowedLabels = []string{"kind:kubernetes"}
	policy.RequiredLabels = []string{"kind:kubernetes"}
	_, ok := authorizedKubernetesRequest(api.AgentEnrollRequest{Labels: []string{"pool:admin"}}, policy)
	assert.False(t, ok, "a label outside the policy's allowed set must be rejected")

	// Capabilities are the agent's own auto-detected advertisement, not an
	// enrollment-policy boundary: requested capabilities never cause rejection.
	_, ok = authorizedKubernetesRequest(api.AgentEnrollRequest{Labels: []string{"kind:kubernetes"}, Capabilities: []string{"pod", "container"}}, policy)
	assert.True(t, ok, "auto-detected capabilities must not be authorized against the policy")
}

func kubernetesEnrollmentPolicy() store.AgentEnrollmentPolicy {
	return store.AgentEnrollmentPolicy{Provider: "kubernetes", SubjectConstraints: json.RawMessage(`{"namespaces":["unified-cd"],"serviceAccounts":["unified-cd-k8s-agent"]}`)}
}
func boundPod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-0", Namespace: "unified-cd", UID: "pod-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "unified-cd-k8s-agent"}}
}

// tokenReviewReactor mirrors what a real API server returns for a projected,
// pod-bound ServiceAccount token: the subject's UID in UserInfo.UID, plus the
// extras a live cluster actually publishes. There is deliberately NO
// "authentication.kubernetes.io/serviceaccount.uid" extra — Kubernetes does not
// emit one, and a fake that invents it cannot catch a verifier reading the
// wrong field. Keep this helper faithful to the API server, never to the
// implementation under test.
func tokenReviewReactor(authenticated bool, audiences []string, username, serviceAccountUID string) k8stesting.ReactionFunc {
	return func(action k8stesting.Action) (bool, runtime.Object, error) {
		user := authv1.UserInfo{Username: username, UID: serviceAccountUID, Extra: serviceAccountTokenExtras()}
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: authenticated, Audiences: audiences, User: user}}, nil
	}
}

// serviceAccountTokenExtras is the complete extras set observed on a live
// Kubernetes v1.35.1 TokenReview for a pod-bound projected ServiceAccount
// token, via both `kubectl create token --bound-object-kind Pod` and a
// serviceAccountToken projected volume read from inside the pod.
func serviceAccountTokenExtras() map[string]authv1.ExtraValue {
	return map[string]authv1.ExtraValue{
		"authentication.kubernetes.io/credential-id": {"JTI=2f1b7d4a-0a11-4c2e-9f0b-6d7c8e9a0b1c"},
		"authentication.kubernetes.io/node-name":     {"node-0"},
		"authentication.kubernetes.io/node-uid":      {"node-uid"},
		"authentication.kubernetes.io/pod-name":      {"agent-0"},
		"authentication.kubernetes.io/pod-uid":       {"pod-uid"},
	}
}
func projectedToken(namespace, serviceAccount, serviceAccountUID, podName, podUID string) string {
	payload, _ := json.Marshal(map[string]any{"kubernetes.io": map[string]any{"namespace": namespace, "serviceaccount": map[string]string{"name": serviceAccount, "uid": serviceAccountUID}, "pod": map[string]string{"name": podName, "uid": podUID}}})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
