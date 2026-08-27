package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"
)

// testProjectedTokenAudience is deliberately NOT KubernetesEnrollmentAudience:
// these tests exercise VerifyProjectedToken as the audience-parameterized
// primitive a second caller (store-credential brokering) will use with its
// own, different audience.
const testProjectedTokenAudience = "unified-cd-test-audience"

// A token minted for one audience must not verify against another. This is
// what stops a job Pod's store-credential token from enrolling an agent, and
// an agent's enrollment token from fetching store credentials.
func TestVerifyProjectedToken_RejectsAWrongAudience(t *testing.T) {
	client := fake.NewSimpleClientset(boundPod())
	client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, []string{"some-other-audience"}, "system:serviceaccount:unified-cd:unified-cd-k8s-agent", "sa-uid"))

	_, err := VerifyProjectedToken(t.Context(), client, "prod", testProjectedTokenAudience, projectedToken("unified-cd", "unified-cd-k8s-agent", "sa-uid", "agent-0", "pod-uid"), 5*time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrKubernetesEnrollmentRejected)
}

// The API server's answer is what counts, not what we asked for: a review that
// authenticates but does not echo the audience must be rejected.
func TestVerifyProjectedToken_RejectsUnconfirmedAudience(t *testing.T) {
	client := fake.NewSimpleClientset(boundPod())
	client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, nil, "system:serviceaccount:unified-cd:unified-cd-k8s-agent", "sa-uid"))

	_, err := VerifyProjectedToken(t.Context(), client, "prod", testProjectedTokenAudience, projectedToken("unified-cd", "unified-cd-k8s-agent", "sa-uid", "agent-0", "pod-uid"), 5*time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrKubernetesEnrollmentRejected)
}

// The reviewed subject must match the token's own claims.
func TestVerifyProjectedToken_RejectsSubjectMismatch(t *testing.T) {
	client := fake.NewSimpleClientset(boundPod())
	client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, []string{testProjectedTokenAudience}, "system:serviceaccount:unified-cd:some-other-account", "sa-uid"))

	_, err := VerifyProjectedToken(t.Context(), client, "prod", testProjectedTokenAudience, projectedToken("unified-cd", "unified-cd-k8s-agent", "sa-uid", "agent-0", "pod-uid"), 5*time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrKubernetesEnrollmentRejected)
}

// A recreated ServiceAccount of the same name has a new UID; a token minted
// for the old one must not verify.
func TestVerifyProjectedToken_RejectsStaleServiceAccountUID(t *testing.T) {
	client := fake.NewSimpleClientset(boundPod())
	client.Fake.PrependReactor("create", "tokenreviews", tokenReviewReactor(true, []string{testProjectedTokenAudience}, "system:serviceaccount:unified-cd:unified-cd-k8s-agent", "new-sa-uid"))

	_, err := VerifyProjectedToken(t.Context(), client, "prod", testProjectedTokenAudience, projectedToken("unified-cd", "unified-cd-k8s-agent", "sa-uid", "agent-0", "pod-uid"), 5*time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrKubernetesEnrollmentRejected)
}
