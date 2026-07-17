package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
	authv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const KubernetesEnrollmentAudience = "unified-cd-agent-enrollment"
const KubernetesEnrollmentRequestTimeout = 5 * time.Second

var (
	ErrKubernetesEnrollmentRejected    = errors.New("kubernetes enrollment rejected")
	ErrKubernetesEnrollmentUnavailable = errors.New("kubernetes identity unavailable")
)

type KubernetesEnrollmentVerifier interface {
	Verify(context.Context, string, store.AgentEnrollmentPolicy) (KubernetesEnrollmentIdentity, error)
}
type KubernetesEnrollmentIdentity struct{ Cluster, Namespace, ServiceAccount, PodName, PodUID string }
type kubernetesEnrollmentVerifier struct {
	cluster        string
	client         kubernetes.Interface
	requestTimeout time.Duration
}

func NewKubernetesEnrollmentVerifier(cluster string, client kubernetes.Interface) KubernetesEnrollmentVerifier {
	return &kubernetesEnrollmentVerifier{cluster: cluster, client: client, requestTimeout: KubernetesEnrollmentRequestTimeout}
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
type kubernetesConstraints struct {
	Namespaces      []string `json:"namespaces"`
	ServiceAccounts []string `json:"serviceAccounts"`
}

func (v *kubernetesEnrollmentVerifier) Verify(ctx context.Context, token string, policy store.AgentEnrollmentPolicy) (KubernetesEnrollmentIdentity, error) {
	if v == nil || v.client == nil {
		return KubernetesEnrollmentIdentity{}, ErrKubernetesEnrollmentUnavailable
	}
	var constraints kubernetesConstraints
	if json.Unmarshal(policy.SubjectConstraints, &constraints) != nil || len(constraints.Namespaces) == 0 || len(constraints.ServiceAccounts) == 0 {
		return KubernetesEnrollmentIdentity{}, fmt.Errorf("%w: policy constraints", ErrKubernetesEnrollmentRejected)
	}
	reviewCtx, cancel := context.WithTimeout(ctx, v.requestTimeout)
	defer cancel()
	review, err := v.client.AuthenticationV1().TokenReviews().Create(reviewCtx, &authv1.TokenReview{Spec: authv1.TokenReviewSpec{Token: token, Audiences: []string{KubernetesEnrollmentAudience}}}, metav1.CreateOptions{})
	if err != nil {
		return KubernetesEnrollmentIdentity{}, fmt.Errorf("%w: token review: %w", ErrKubernetesEnrollmentUnavailable, err)
	}
	if !review.Status.Authenticated || !contains(review.Status.Audiences, KubernetesEnrollmentAudience) {
		return KubernetesEnrollmentIdentity{}, fmt.Errorf("%w: token review", ErrKubernetesEnrollmentRejected)
	}
	claims, err := parseBoundPodClaims(token)
	if err != nil {
		return KubernetesEnrollmentIdentity{}, fmt.Errorf("%w: projected token claims", ErrKubernetesEnrollmentRejected)
	}
	if review.Status.User.Username != "system:serviceaccount:"+claims.Namespace+":"+claims.ServiceAccount.Name {
		return KubernetesEnrollmentIdentity{}, fmt.Errorf("%w: token review subject", ErrKubernetesEnrollmentRejected)
	}
	if reviewedUID := review.Status.User.Extra["authentication.kubernetes.io/serviceaccount.uid"]; len(reviewedUID) > 0 && (claims.ServiceAccount.UID == "" || !contains([]string(reviewedUID), claims.ServiceAccount.UID)) {
		return KubernetesEnrollmentIdentity{}, fmt.Errorf("%w: token review service account UID", ErrKubernetesEnrollmentRejected)
	}
	if !contains(constraints.Namespaces, claims.Namespace) || !contains(constraints.ServiceAccounts, claims.ServiceAccount.Name) {
		return KubernetesEnrollmentIdentity{}, fmt.Errorf("%w: policy subject", ErrKubernetesEnrollmentRejected)
	}
	podCtx, cancel := context.WithTimeout(ctx, v.requestTimeout)
	defer cancel()
	pod, err := v.client.CoreV1().Pods(claims.Namespace).Get(podCtx, claims.Pod.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return KubernetesEnrollmentIdentity{}, fmt.Errorf("%w: pod", ErrKubernetesEnrollmentRejected)
		}
		return KubernetesEnrollmentIdentity{}, fmt.Errorf("%w: pod: %w", ErrKubernetesEnrollmentUnavailable, err)
	}
	if string(pod.UID) != claims.Pod.UID || pod.Namespace != claims.Namespace || pod.Name != claims.Pod.Name || pod.Spec.ServiceAccountName != claims.ServiceAccount.Name {
		return KubernetesEnrollmentIdentity{}, fmt.Errorf("%w: pod binding", ErrKubernetesEnrollmentRejected)
	}
	return KubernetesEnrollmentIdentity{Cluster: v.cluster, Namespace: claims.Namespace, ServiceAccount: claims.ServiceAccount.Name, PodName: claims.Pod.Name, PodUID: claims.Pod.UID}, nil
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
	if result.Namespace == "" || result.ServiceAccount.Name == "" || result.Pod.Name == "" || result.Pod.UID == "" {
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
