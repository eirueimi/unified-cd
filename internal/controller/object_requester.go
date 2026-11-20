package controller

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"k8s.io/client-go/kubernetes"
)

// KubernetesStoreCredentialRequestTimeout bounds the TokenReview call made
// per configured cluster when a job Pod's sidecar authenticates to the object
// endpoints. A sibling of KubernetesEnrollmentRequestTimeout, kept separate
// rather than shared: the two audiences are a deliberate security boundary
// (see api.KubernetesStoreCredentialAudience's doc comment), and the sidecar
// path is called far more often than enrollment, so each has its own timeout.
const KubernetesStoreCredentialRequestTimeout = 5 * time.Second

// StoreCredentialCluster names one Kubernetes cluster trusted to authenticate
// job Pod sidecars on the object endpoints, and which namespaces and
// ServiceAccounts within it may do so. The name predates the presign design
// and is kept because it is the config key (agentAuth.
// kubernetesStoreCredentialPolicies) operators already have.
//
// This is server CONFIGURATION, not a database-backed policy like
// KubernetesEnrollmentVerifiers' constraints:
//
//  1. A sidecar presents only a token, so the handler has no policy name to
//     look one up by. It tries each configured cluster's client until one
//     TokenReview authenticates (safe: a token reviewed against the wrong
//     cluster's API server comes back unauthenticated, it does not error).
//  2. The agent's OWN kubernetes enrollment policy names the AGENT's
//     namespace/ServiceAccount, not the JOB POD's (a different namespace by
//     design). Reusing it would reject the common case, so job-Pod identity
//     and agent identity stay two separate trust domains.
type StoreCredentialCluster struct {
	Cluster         string
	Client          kubernetes.Interface
	Namespaces      []string
	ServiceAccounts []string
}

// verifyStoreCredentialToken tries every configured cluster's client in turn
// and returns the first that authenticates the token against
// api.KubernetesStoreCredentialAudience, along with the StoreCredentialCluster
// entry that succeeded (needed afterward for its namespace/ServiceAccount
// allowlist). In the common single-cluster deployment this tries exactly one
// client.
//
// The last error observed is returned when every cluster rejects the token,
// so a genuinely unavailable TokenReview is reported as such rather than
// folded into "rejected".
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
// even ask" class of failure (a TokenReview/Pod-lookup timeout, or no cluster
// configured at all) rather than "we asked, and the token is not valid".
func isKubernetesIdentityUnavailable(err error) bool {
	return errors.Is(err, ErrKubernetesEnrollmentUnavailable)
}

// presignRateProvider names the presign endpoint's buckets in objectLimiter,
// so they never share a budget with another endpoint.
const presignRateProvider = "objects-presign"

// objectRequester is who asked the presign endpoint for a URL: an agent
// (AgentID set) or a Kubernetes job Pod's artifact sidecar (PodUID set).
type objectRequester struct {
	AgentID string
	PodUID  string
	// RunID is the run in the request path, checked to exist.
	RunID string
	// JobName is the run record's job (store.RunJobName), the only source of
	// the cache namespace.
	JobName string
}

// authenticateObjectRequester authenticates a presign request for runID and
// checks that the requester may act for that run. On failure it has already
// written the response and returns false. Only 403 refusals are audited: they
// come from an authenticated requester after its rate limit, so they cannot
// grow the audit log at request rate the way 401s and 429s could (a 401 from
// an agent bearer is still logged by authenticateAgentBearer).
//
//   - An agent bearer (uca_) is authenticated like agentAuth, rate-limited by
//     agent ID, and must have claimed the run (agentRunGuard without the
//     terminal check, as handleArtifactUpload: a finished run still accepts
//     late uploads).
//   - Any other bearer is a job Pod's projected ServiceAccount token. It is
//     rate-limited by client address before the TokenReview, must come from
//     an allow-listed namespace and ServiceAccount, and the Pod must be the
//     one bound to the run. A missing binding is 409 binding-pending (the
//     agent reports it on its next heartbeat; the sidecar retries).
//     The binding is always required: a URL is for one run's object.
func (s *Server) authenticateObjectRequester(w http.ResponseWriter, r *http.Request, runID string) (objectRequester, bool) {
	return s.authenticateObjectRequesterAs(w, r, runID, auditActionObjectsPresign)
}

// authenticateObjectRequesterAs is authenticateObjectRequester with the audit
// action its 403 rows are written under (the streaming routes use
// auditActionObjectsStream).
func (s *Server) authenticateObjectRequesterAs(w http.ResponseWriter, r *http.Request, runID, action string) (objectRequester, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, bearerPrefix) || strings.TrimPrefix(header, bearerPrefix) == "" {
		writeObjectError(w, http.StatusUnauthorized, api.ObjectErrUnauthenticated, "missing bearer token")
		return objectRequester{}, false
	}
	token := strings.TrimPrefix(header, bearerPrefix)
	if strings.HasPrefix(token, "uca_") {
		return s.authenticateObjectAgent(w, r, runID, action)
	}
	return s.authenticateObjectSidecar(w, r, runID, token, action)
}

func (s *Server) authenticateObjectAgent(w http.ResponseWriter, r *http.Request, runID, action string) (objectRequester, bool) {
	principal, status, _ := s.authenticateAgentBearer(r)
	if status != 0 {
		if status == http.StatusServiceUnavailable {
			writeObjectError(w, status, api.ObjectErrUnavailable, "authentication unavailable")
			return objectRequester{}, false
		}
		writeObjectError(w, http.StatusUnauthorized, api.ObjectErrUnauthenticated, "unauthorized")
		return objectRequester{}, false
	}
	req := objectRequester{AgentID: principal.AgentID}
	if s.objectLimiter != nil && !s.objectLimiter.allowKey(presignRateProvider+"\x00agent\x00"+principal.AgentID) {
		w.Header().Set("Retry-After", "1")
		writeObjectError(w, http.StatusTooManyRequests, api.ObjectErrRateLimited, "presign rate limit exceeded")
		return objectRequester{}, false
	}
	if !s.resolveRequesterRun(w, r, runID, &req) {
		return objectRequester{}, false
	}
	verdict, err := s.agentRunGuard(r.Context(), principal.AgentID, runID, false)
	if err != nil {
		slog.Warn("presign: run ownership check failed", "runId", runID, "error", err)
		writeObjectError(w, http.StatusInternalServerError, api.ObjectErrInternal, "run ownership check failed")
		return objectRequester{}, false
	}
	switch verdict {
	case runWriteOK:
		return req, true
	case runWriteNotFound:
		writeObjectError(w, http.StatusNotFound, api.ObjectErrNotFound, "run not found")
	default:
		s.auditPresignAction(r, req, action, runID, nil, http.StatusForbidden)
		writeObjectError(w, http.StatusForbidden, api.ObjectErrForbidden, "run "+runID+" is claimed by another agent")
	}
	return objectRequester{}, false
}

func (s *Server) authenticateObjectSidecar(w http.ResponseWriter, r *http.Request, runID, token, action string) (objectRequester, bool) {
	// The TokenReview is a call to the cluster API: bound the rate per client
	// address before doing any work.
	if s.objectLimiter != nil && !s.objectLimiter.allow(r, presignRateProvider, "sidecar") {
		w.Header().Set("Retry-After", "1")
		writeObjectError(w, http.StatusTooManyRequests, api.ObjectErrRateLimited, "presign rate limit exceeded")
		return objectRequester{}, false
	}
	if len(s.storeCredentialClusters) == 0 {
		// No cluster is trusted: a non-agent bearer cannot be anything valid.
		writeObjectError(w, http.StatusUnauthorized, api.ObjectErrUnauthenticated, "unauthorized")
		return objectRequester{}, false
	}
	identity, cluster, err := s.verifyStoreCredentialToken(r, token)
	if err != nil {
		if isKubernetesIdentityUnavailable(err) {
			writeObjectError(w, http.StatusServiceUnavailable, api.ObjectErrUnavailable, "kubernetes identity unavailable")
			return objectRequester{}, false
		}
		writeObjectError(w, http.StatusUnauthorized, api.ObjectErrUnauthenticated, "unauthorized")
		return objectRequester{}, false
	}
	req := objectRequester{PodUID: identity.PodUID}
	if !contains(cluster.Namespaces, identity.Namespace) || !contains(cluster.ServiceAccounts, identity.ServiceAccount) {
		s.auditPresignAction(r, req, action, runID, nil, http.StatusForbidden)
		writeObjectError(w, http.StatusForbidden, api.ObjectErrForbidden, "this namespace or service account may not request object URLs")
		return objectRequester{}, false
	}
	if !s.resolveRequesterRun(w, r, runID, &req) {
		return objectRequester{}, false
	}
	// Cancel, the reaper and the scheduler finish a run without deleting its
	// bindings (only the agent's FinishRun does), so a still-alive Pod of a
	// finished run is refused by status. The agent path deliberately accepts
	// late uploads; a Pod's access ends with the run.
	run, err := s.store.GetRun(r.Context(), runID)
	if errors.Is(err, store.ErrRunNotFound) {
		writeObjectError(w, http.StatusNotFound, api.ObjectErrNotFound, "run not found")
		return objectRequester{}, false
	}
	if err != nil {
		slog.Warn("presign: run status lookup failed", "runId", runID, "error", err)
		writeObjectError(w, http.StatusInternalServerError, api.ObjectErrInternal, "run status lookup failed")
		return objectRequester{}, false
	}
	if isTerminalStatus(string(run.Status)) {
		s.auditPresignAction(r, req, action, runID, nil, http.StatusConflict)
		writeObjectError(w, http.StatusConflict, api.ObjectErrRunFinished, "run "+runID+" has finished")
		return objectRequester{}, false
	}
	bindings, err := s.store.ListRunPodBindings(r.Context(), runID)
	if err != nil {
		slog.Warn("presign: run/pod binding lookup failed", "runId", runID, "error", err)
		writeObjectError(w, http.StatusInternalServerError, api.ObjectErrInternal, "run/pod binding lookup failed")
		return objectRequester{}, false
	}
	if len(bindings) == 0 {
		w.Header().Set("Retry-After", "1")
		writeObjectError(w, http.StatusConflict, api.ObjectErrBindingPending, "no pod is bound to this run yet; retry")
		return objectRequester{}, false
	}
	bound := false
	for _, b := range bindings {
		if b.PodUID == identity.PodUID {
			bound = true
			break
		}
	}
	if !bound {
		s.auditPresignAction(r, req, action, runID, nil, http.StatusForbidden)
		writeObjectError(w, http.StatusForbidden, api.ObjectErrForbidden, "the presented pod is not the one bound to this run")
		return objectRequester{}, false
	}
	return req, true
}

// resolveRequesterRun checks that runID exists and records its job name. A
// malformed run ID is a 404, like an unknown one.
func (s *Server) resolveRequesterRun(w http.ResponseWriter, r *http.Request, runID string, req *objectRequester) bool {
	if s.store == nil {
		writeObjectError(w, http.StatusServiceUnavailable, api.ObjectErrUnavailable, "store not configured")
		return false
	}
	job, found, err := s.store.RunJobName(r.Context(), runID)
	if err != nil {
		slog.Warn("presign: run lookup failed", "runId", runID, "error", err)
		writeObjectError(w, http.StatusInternalServerError, api.ObjectErrInternal, "run lookup failed")
		return false
	}
	if !found || job == "" {
		writeObjectError(w, http.StatusNotFound, api.ObjectErrNotFound, "run not found")
		return false
	}
	req.RunID = runID
	req.JobName = job
	return true
}

func writeObjectError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, api.ObjectError{Error: msg, Code: code})
}
