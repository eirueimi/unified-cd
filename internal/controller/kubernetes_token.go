package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// BoundPodIdentity is the identity a projected ServiceAccount token proves.
type BoundPodIdentity struct {
	Cluster, Namespace, ServiceAccount, PodName, PodUID string
}

type projectedServiceAccountClaims struct {
	Kubernetes map[string]json.RawMessage `json:"kubernetes.io"`
}
type boundPodClaims struct {
	Namespace      string `json:"namespace"`
	ServiceAccount struct {
		Name string `json:"name"`
		UID  string `json:"uid"`
	} `json:"serviceaccount"`
	Pod struct {
		Name string `json:"name"`
		UID  string `json:"uid"`
	} `json:"pod"`
}

// VerifyProjectedToken confirms a projected ServiceAccount token against one
// audience and returns the Pod identity it proves.
//
// The audience is a parameter and not a constant because two callers use this
// with DIFFERENT audiences, and that difference is a security boundary: agent
// enrollment and store-credential brokering must never accept each other's
// tokens. A shared audience would let any job Pod register itself as an agent.
func VerifyProjectedToken(ctx context.Context, client kubernetes.Interface, cluster, audience, token string, timeout time.Duration) (BoundPodIdentity, error) {
	if client == nil {
		return BoundPodIdentity{}, ErrKubernetesEnrollmentUnavailable
	}
	reviewCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	review, err := client.AuthenticationV1().TokenReviews().Create(reviewCtx, &authv1.TokenReview{Spec: authv1.TokenReviewSpec{Token: token, Audiences: []string{audience}}}, metav1.CreateOptions{})
	if err != nil {
		return BoundPodIdentity{}, fmt.Errorf("%w: token review: %w", ErrKubernetesEnrollmentUnavailable, err)
	}
	if !review.Status.Authenticated || !contains(review.Status.Audiences, audience) {
		return BoundPodIdentity{}, fmt.Errorf("%w: token review", ErrKubernetesEnrollmentRejected)
	}
	claims, err := parseBoundPodClaims(token)
	if err != nil {
		return BoundPodIdentity{}, fmt.Errorf("%w: projected token claims", ErrKubernetesEnrollmentRejected)
	}
	if review.Status.User.Username != "system:serviceaccount:"+claims.Namespace+":"+claims.ServiceAccount.Name {
		return BoundPodIdentity{}, fmt.Errorf("%w: token review subject", ErrKubernetesEnrollmentRejected)
	}
	// The API server returns the authenticated subject's UID in Status.User.UID.
	// It does NOT publish a "authentication.kubernetes.io/serviceaccount.uid"
	// entry in Status.User.Extra — the extras a projected ServiceAccount token
	// carries are credential-id, node-name, node-uid, pod-name and pod-uid.
	// Binding the token's own serviceaccount.uid claim to the reviewed UID is
	// what stops a token minted for a deleted-and-recreated ServiceAccount of
	// the same name from being accepted, so the comparison itself is kept.
	reviewedUID := review.Status.User.UID
	if claims.ServiceAccount.UID == "" || reviewedUID == "" || reviewedUID != claims.ServiceAccount.UID {
		return BoundPodIdentity{}, fmt.Errorf("%w: token review service account UID", ErrKubernetesEnrollmentRejected)
	}
	// The TokenReview and the claims establish that the token was minted for
	// this ServiceAccount, but say nothing about whether the Pod it was bound
	// to still exists. A live fetch, compared field-by-field against the
	// claims, is what stops a token minted for a deleted-and-recreated Pod of
	// the same name from being accepted — the same freshness guarantee the
	// ServiceAccount-UID check above provides for the ServiceAccount. Both
	// enrollment and store-credential brokering need this, so it lives here
	// rather than in a single caller's policy step.
	podCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	pod, err := client.CoreV1().Pods(claims.Namespace).Get(podCtx, claims.Pod.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return BoundPodIdentity{}, fmt.Errorf("%w: pod", ErrKubernetesEnrollmentRejected)
		}
		return BoundPodIdentity{}, fmt.Errorf("%w: pod: %w", ErrKubernetesEnrollmentUnavailable, err)
	}
	if string(pod.UID) != claims.Pod.UID || pod.Namespace != claims.Namespace || pod.Name != claims.Pod.Name || pod.Spec.ServiceAccountName != claims.ServiceAccount.Name {
		return BoundPodIdentity{}, fmt.Errorf("%w: pod binding", ErrKubernetesEnrollmentRejected)
	}
	return BoundPodIdentity{Cluster: cluster, Namespace: claims.Namespace, ServiceAccount: claims.ServiceAccount.Name, PodName: claims.Pod.Name, PodUID: claims.Pod.UID}, nil
}

func parseBoundPodClaims(token string) (boundPodClaims, error) {
	var result boundPodClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return result, errors.New("malformed token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return result, err
	}
	var envelope projectedServiceAccountClaims
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return result, err
	}
	raw := envelope.Kubernetes["pod"]
	_ = raw
	if len(envelope.Kubernetes) == 0 {
		return result, errors.New("no kubernetes claims")
	}
	b, _ := json.Marshal(envelope.Kubernetes)
	if err := json.Unmarshal(b, &result); err != nil {
		return result, err
	}
	if result.Namespace == "" || result.ServiceAccount.Name == "" || result.ServiceAccount.UID == "" || result.Pod.Name == "" || result.Pod.UID == "" {
		return result, errors.New("incomplete binding")
	}
	return result, nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
