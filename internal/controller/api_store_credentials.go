package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"k8s.io/client-go/kubernetes"
)

// KubernetesStoreCredentialRequestTimeout bounds the TokenReview call the
// broker makes per configured cluster. A sibling of
// KubernetesEnrollmentRequestTimeout, kept separate rather than shared: the
// two audiences are a deliberate security boundary (see
// api.KubernetesStoreCredentialAudience's doc comment), and giving them
// independently-named timeouts means changing one later — e.g. because the
// broker is called far more often, on the hot path of every job's artifact
// step, while enrollment happens once per agent lifetime — never requires
// touching the other's callers to reason about what changed.
const KubernetesStoreCredentialRequestTimeout = 5 * time.Second

// StoreCredentialCluster names one Kubernetes cluster trusted to broker
// object-store credentials to job Pod sidecars, and which namespaces and
// ServiceAccounts within it may ask for them.
//
// This is server CONFIGURATION, not a database-backed policy — unlike
// KubernetesEnrollmentVerifiers' namespace/ServiceAccount constraints, which
// come from a store.AgentEnrollmentPolicy an admin creates and rotates
// through the API. Two reasons that mechanism is the wrong fit here rather
// than reused as-is:
//
//  1. api.StoreCredentialsRequest carries only a token and a RunID — no
//     policy name — so the handler has no key to look up a specific named
//     policy by. It must instead try each configured cluster's client until
//     one TokenReview authenticates (safe: a token reviewed against the
//     wrong cluster's API server simply comes back unauthenticated, it does
//     not error), which is a natural fit for a short, static list but not
//     for a store lookup keyed by name.
//  2. The agent's OWN kubernetes-provider AgentEnrollmentPolicy names the
//     AGENT's namespace/ServiceAccount (e.g. "unified-cd"/"k8s-agent" in the
//     shipped manifests) — not the JOB POD's (e.g. "ci"/whatever runs jobs,
//     a different namespace by design; see
//     docs/operator-manual/kubernetes-integration.md's "S3 credentials"
//     section on exactly this distinction already tripping operators up for
//     the Secret path this broker replaces). Reusing that policy record
//     verbatim for store-credential authorization would reject the common
//     case, not merely fail to restrict it. A same-shaped-but-separate list
//     keeps job-Pod identity and agent identity as what they are: two
//     different trust domains, not one admin-managed, audited object
//     wearing two hats.
type StoreCredentialCluster struct {
	Cluster         string
	Client          kubernetes.Interface
	Namespaces      []string
	ServiceAccounts []string
}

// handleStoreCredentials is the §5.6 broker endpoint: a job Pod's sidecar
// presents its projected ServiceAccount token and gets back the credentials
// to build an object-store client with. See
// docs/superpowers/specs/2026-08-26-sidecar-credential-delivery-design.md
// §5.6 and docs/superpowers/plans/2026-08-26-controller-brokered-store-credentials.md
// Task 2.
func (s *Server) handleStoreCredentials(w http.ResponseWriter, r *http.Request) {
	var req api.StoreCredentialsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		s.auditAgentCredential(r, "store-credentials.fetch", "", http.StatusBadRequest)
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	identity, cluster, err := s.verifyStoreCredentialToken(r, req.Token)
	if err != nil {
		if isKubernetesIdentityUnavailable(err) {
			s.auditAgentCredential(r, "store-credentials.fetch", "", http.StatusServiceUnavailable)
			http.Error(w, "kubernetes identity unavailable", http.StatusServiceUnavailable)
			return
		}
		s.auditAgentCredential(r, "store-credentials.fetch", "", http.StatusForbidden)
		http.Error(w, "store credentials rejected", http.StatusForbidden)
		return
	}

	if !contains(cluster.Namespaces, identity.Namespace) || !contains(cluster.ServiceAccounts, identity.ServiceAccount) {
		s.auditAgentCredential(r, "store-credentials.fetch", identity.PodUID, http.StatusForbidden)
		http.Error(w, "store credentials rejected", http.StatusForbidden)
		return
	}

	if s.storeCredentialS3 == nil {
		s.auditAgentCredential(r, "store-credentials.fetch", identity.PodUID, http.StatusServiceUnavailable)
		http.Error(w, "store credentials unavailable: the controller has no object store configured (set UNIFIED_S3_ENDPOINT and UNIFIED_S3_BUCKET)", http.StatusServiceUnavailable)
		return
	}

	// Return the controller's OWN credential as-is. Do not scope it.
	//
	//   - Scoping to a run's object-store prefix needs STS support that is
	//     not universal — the shipped evaluation bundle uses Garage, whose
	//     AssumeRoleWithWebIdentity support is unconfirmed (spec §9).
	//     Passthrough works on every store this project supports today.
	//   - Passthrough leaves the blast radius exactly where it is today
	//     (every job's sidecar already effectively shared one bucket-scoped
	//     credential, via the operator-managed Secret this broker
	//     replaces), while removing the operator's per-namespace Secret —
	//     which is the reported operator-facing problem, and is complete on
	//     its own without scoping.
	//   - api.StoreCredentialsResponse's ExpiresAt/sessionToken fields exist
	//     so that adding scoping later is a change of what the controller
	//     RETURNS here, not a change of what any sidecar PARSES — this
	//     handler is the only place that changes when that happens.
	//
	// identity.PodUID is not enforced against req.RunID here: the token
	// proves the Pod's namespace/ServiceAccount/UID, not which run it is
	// executing, and with a passthrough credential every caller gets the
	// identical value regardless — binding the request to a specific run
	// would buy no isolation yet. It becomes necessary once a credential
	// CAN be scoped per run, at which point the agent has to tell the
	// controller which Pod runs which run and this handler starts checking
	// it. See api.StoreCredentialsRequest.RunID's doc comment.
	writeJSON(w, http.StatusOK, api.StoreCredentialsResponse{
		Endpoint:  s.storeCredentialS3.Endpoint,
		Bucket:    s.storeCredentialS3.Bucket,
		Region:    s.storeCredentialS3.Region,
		UseSSL:    s.storeCredentialS3.UseSSL,
		AccessKey: s.storeCredentialS3.AccessKeyID,
		SecretKey: s.storeCredentialS3.SecretAccessKey,
	})
	s.auditAgentCredential(r, "store-credentials.fetch", identity.PodUID, http.StatusOK)
}

// verifyStoreCredentialToken tries every configured cluster's client in
// turn and returns the first that authenticates the token against
// api.KubernetesStoreCredentialAudience, along with the StoreCredentialCluster
// entry that succeeded (needed afterward for its namespace/ServiceAccount
// allowlist). Trying every cluster is safe, not merely convenient:
// api.StoreCredentialsRequest carries no cluster name for the handler to
// pick one by, and a TokenReview against the WRONG cluster's API server
// comes back unauthenticated rather than erroring — a token cryptographically
// bound to cluster A is not a valid token to cluster B's API server. In the
// common single-cluster deployment this tries exactly one client.
//
// The last error observed is returned when every cluster rejects the token,
// so a genuinely unavailable TokenReview (a real API-server timeout) is
// reported as such rather than folded into "rejected" — the caller maps
// ErrKubernetesEnrollmentUnavailable to 503 and everything else to 403.
func (s *Server) verifyStoreCredentialToken(r *http.Request, token string) (BoundPodIdentity, StoreCredentialCluster, error) {
	if len(s.storeCredentialClusters) == 0 {
		return BoundPodIdentity{}, StoreCredentialCluster{}, ErrKubernetesEnrollmentUnavailable
	}
	var lastErr error
	for _, cluster := range s.storeCredentialClusters {
		identity, err := VerifyProjectedToken(r.Context(), cluster.Client, cluster.Cluster, api.KubernetesStoreCredentialAudience, token, KubernetesStoreCredentialRequestTimeout)
		if err == nil {
			return identity, cluster, nil
		}
		lastErr = err
	}
	return BoundPodIdentity{}, StoreCredentialCluster{}, lastErr
}

// isKubernetesIdentityUnavailable reports whether err is the "we could not
// even ask" class of failure (a TokenReview/Pod-lookup timeout, or no
// cluster configured at all) rather than "we asked, and the token is not
// valid" — the same distinction handleKubernetesAgentEnroll draws between
// ErrKubernetesEnrollmentUnavailable (503) and everything else (403/other).
func isKubernetesIdentityUnavailable(err error) bool {
	return errors.Is(err, ErrKubernetesEnrollmentUnavailable)
}
