package controller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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

	if rejected, reason := s.runBindingRejects(r.Context(), req.RunID, identity); rejected {
		s.auditAgentCredential(r, "store-credentials.fetch", identity.PodUID, http.StatusForbidden)
		http.Error(w, "store credentials rejected: "+reason, http.StatusForbidden)
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
	// identity.PodUID WAS already checked against req.RunID above, by
	// runBindingRejects — but the credential returned below is still the
	// controller's own, unscoped one: the token proves the Pod's
	// namespace/ServiceAccount/UID, and the binding check above proves it is
	// the Pod executing RunID, but with a PASSTHROUGH credential every
	// caller gets the identical value regardless, so there is nothing here
	// for the binding to scope YET. The check is still worth doing now
	// (it narrows a stolen/misdirected token to the one run it was minted
	// for) and is the prerequisite for the day a credential CAN be scoped
	// per run — see api.StoreCredentialsRequest.RunID's doc comment.
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

// runBindingRejects reports whether a store-credentials request naming
// runID, from a Pod whose identity has ALREADY been verified by TokenReview
// (identity), must be refused for binding reasons — i.e. because runID
// names a run some OTHER Pod is executing.
//
// There are three cases, and only one of them is unconditional:
//
//  1. runID == "": nothing to check identity against (an old sidecar built
//     before RunID was threaded through cmd/unified-sidecar/run.go, or any
//     other caller that simply omits it). Cannot enforce — falls to
//     RequireRunBinding.
//  2. No binding is recorded for runID yet (store.GetRunPodBinding's
//     ok == false, or s.store itself is nil in a narrow unit test): the
//     same "cannot enforce" bucket as (1) — a k8s agent heartbeats this
//     binding on its own interval (HeartbeatRequest.PodBindings), so a
//     freshly-created Pod's sidecar can legitimately ask before its first
//     heartbeat has landed, and a pre-this-feature k8s agent binary never
//     reports one at all. Falls to RequireRunBinding.
//  3. A binding IS recorded and names a DIFFERENT Pod (by PodUID — see
//     api.PodBinding's doc comment): this is not missing information, it
//     is a contradiction of it, and is refused UNCONDITIONALLY — the one
//     case RequireRunBinding does not gate, because permissiveness is only
//     ever about not knowing, never about a known mismatch.
//
// See Config.RequireRunBinding for why (1) and (2) default to "allow".
func (s *Server) runBindingRejects(ctx context.Context, runID string, identity BoundPodIdentity) (rejected bool, reason string) {
	if runID == "" {
		return s.cfg.RequireRunBinding, "no run id was presented"
	}
	if s.store == nil {
		return s.cfg.RequireRunBinding, "no run/pod binding store is configured"
	}
	binding, ok, err := s.store.GetRunPodBinding(ctx, runID)
	if err != nil {
		// A transient store failure is "unknown", not "reject regardless of
		// RequireRunBinding": unlike case 3 above, there is no CONTRADICTION
		// here, only an inability to look one up, so it belongs in the same
		// bucket as never having heard of the run at all.
		slog.Warn("store-credentials: run/pod binding lookup failed; treating as unknown", "runId", runID, "error", err)
		return s.cfg.RequireRunBinding, "run/pod binding lookup failed"
	}
	if !ok {
		return s.cfg.RequireRunBinding, "no pod is bound to this run yet"
	}
	if binding.PodUID != identity.PodUID {
		return true, "the presented pod is not the one bound to this run"
	}
	return false, ""
}
