package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
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
	identity, err := VerifyProjectedToken(ctx, v.client, v.cluster, KubernetesEnrollmentAudience, token, v.requestTimeout)
	if err != nil {
		return KubernetesEnrollmentIdentity{}, err
	}
	if !contains(constraints.Namespaces, identity.Namespace) || !contains(constraints.ServiceAccounts, identity.ServiceAccount) {
		return KubernetesEnrollmentIdentity{}, fmt.Errorf("%w: policy subject", ErrKubernetesEnrollmentRejected)
	}
	return KubernetesEnrollmentIdentity{Cluster: identity.Cluster, Namespace: identity.Namespace, ServiceAccount: identity.ServiceAccount, PodName: identity.PodName, PodUID: identity.PodUID}, nil
}
