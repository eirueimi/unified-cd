package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/eirueimi/unified-cd/internal/agentauth"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
)

const (
	gcpOIDCIssuer  = "https://accounts.google.com"
	gcpJWKSURL     = "https://www.googleapis.com/oauth2/v3/certs"
	azureAuthority = "https://login.microsoftonline.com"

	// cloudEnrollmentRequestTimeout bounds each outbound identity lookup
	// (JWKS fetch, STS call) made on behalf of one enrollment request.
	cloudEnrollmentRequestTimeout = 5 * time.Second
)

var (
	// ErrCloudEnrollmentRejected means the proof or the policy match failed.
	ErrCloudEnrollmentRejected = errors.New("enrollment policy rejected")
	// ErrCloudEnrollmentUnavailable means the cloud identity service could not
	// be reached or answered with a server error.
	ErrCloudEnrollmentUnavailable = errors.New("identity unavailable")
)

// CloudEnrollmentProof is the caller-supplied identity evidence. Exactly one
// field is meaningful per provider: BearerToken for gcp and azure, AWS for aws.
type CloudEnrollmentProof struct {
	BearerToken string
	AWS         *api.AWSSignedRequest
}

// CloudEnrollmentIdentity is the verified, canonical identity of the caller.
// AgentID doubles as the ExternalSubject of the stored agent identity.
type CloudEnrollmentIdentity struct{ AgentID string }

// CloudEnrollmentVerifier verifies one cloud provider's identity proof against
// an enrollment policy. Implementations wrap ErrCloudEnrollmentRejected or
// ErrCloudEnrollmentUnavailable; detail is logged by the handler, never
// returned to the caller.
type CloudEnrollmentVerifier interface {
	Verify(ctx context.Context, proof CloudEnrollmentProof, policy store.AgentEnrollmentPolicy) (CloudEnrollmentIdentity, error)
}

// defaultCloudEnrollmentVerifiers returns the real verifiers for the
// providers whose verification is implemented.
func defaultCloudEnrollmentVerifiers() map[string]CloudEnrollmentVerifier {
	return map[string]CloudEnrollmentVerifier{
		"gcp":   NewGCPEnrollmentVerifier(gcpOIDCIssuer, gcpJWKSURL, nil),
		"aws":   NewAWSEnrollmentVerifier(nil, nil),
		"azure": NewAzureEnrollmentVerifier(azureAuthority, nil),
	}
}

func (s *Server) handleCloudAgentEnroll(w http.ResponseWriter, r *http.Request, req api.AgentEnrollRequest) {
	provider := req.Provider
	if !s.allowAgentCredentialRequest(w, r, provider, "agent.enrollment.exchange") {
		return
	}
	unavailable := func() {
		s.recordAgentAuth(provider, "failure", "unavailable")
		http.Error(w, provider+" identity unavailable", http.StatusServiceUnavailable)
	}
	// rejected answers the generic 403. The reason goes to the controller log
	// only (never to the caller) and never includes proof contents.
	rejected := func(policyName, reason string) {
		slog.Warn("cloud enrollment rejected", "provider", provider, "policy", policyName, "reason", reason)
		s.recordAgentAuth(provider, "failure", "policy")
		http.Error(w, "enrollment policy rejected", http.StatusForbidden)
	}
	proof := CloudEnrollmentProof{AWS: req.AWS}
	if provider != "aws" {
		token, ok := bearerToken(r)
		if !ok {
			s.recordAgentAuth(provider, "failure", "invalid")
			http.Error(w, "enrollment policy rejected", http.StatusForbidden)
			return
		}
		proof.BearerToken = token
	} else if req.AWS == nil {
		s.recordAgentAuth(provider, "failure", "invalid")
		http.Error(w, "enrollment policy rejected", http.StatusForbidden)
		return
	}
	if s.store == nil {
		unavailable()
		return
	}
	policy, err := s.store.GetAgentEnrollmentPolicy(r.Context(), req.Policy)
	if err != nil {
		slog.Warn("cloud enrollment policy lookup failed", "provider", provider, "error", err)
		unavailable()
		return
	}
	switch {
	case policy == nil:
		rejected(req.Policy, "policy not found")
		return
	case !policy.Enabled:
		rejected(policy.Name, "policy disabled")
		return
	case policy.Provider != provider:
		rejected(policy.Name, "provider mismatch")
		return
	}
	verifier := s.cloudEnrollmentVerifiers[provider]
	if verifier == nil {
		unavailable()
		return
	}
	id, err := verifier.Verify(r.Context(), proof, *policy)
	if err != nil {
		slog.Warn("cloud enrollment verification failed", "provider", provider, "policy", policy.Name, "error", err)
		if errors.Is(err, ErrCloudEnrollmentUnavailable) {
			unavailable()
		} else {
			s.recordAgentAuth(provider, "failure", "policy")
			http.Error(w, "enrollment policy rejected", http.StatusForbidden)
		}
		return
	}
	labels, ok := authorizedEnrollmentLabels(req, *policy)
	if !ok {
		rejected(policy.Name, "labels not allowed")
		return
	}
	access, err := agentauth.Generate(agentauth.AccessToken)
	if err != nil {
		unavailable()
		return
	}
	now := time.Now()
	agentIdentity, err := s.store.IssueExternalAgentAccess(r.Context(), store.AgentCredentialIssue{
		AgentID: id.AgentID, EnrollmentMethod: provider, ExternalSubject: id.AgentID, AuthorizedLabels: labels,
		Access: newAgentCredential(access, "access", "", 0, now.Add(policy.AccessTokenTTL)),
	})
	if err != nil {
		s.respondAgentCredentialError(r, w, provider, "agent.enrollment.exchange", policy.Name, err)
		return
	}
	s.recordAgentAuth(provider, "success", "ok")
	s.auditAgentCredential(r, "agent.enrollment.exchange", policy.Name, http.StatusOK)
	writeJSON(w, http.StatusOK, api.AgentTokenResponse{AgentID: agentIdentity.AgentID, AccessToken: access.Plaintext, AccessExpiresAt: now.Add(policy.AccessTokenTTL), Labels: agentIdentity.AuthorizedLabels, Capabilities: agentIdentity.AuthorizedCapabilities})
}

type gcpEnrollmentVerifier struct {
	issuer  string
	keySet  oidc.KeySet
	timeout time.Duration
}

// NewGCPEnrollmentVerifier verifies Google Compute Engine instance identity
// tokens (format=full) signed by the keys at jwksURL. A nil client uses a
// default client with a request timeout.
func NewGCPEnrollmentVerifier(issuer, jwksURL string, client *http.Client) CloudEnrollmentVerifier {
	if client == nil {
		client = &http.Client{Timeout: cloudEnrollmentRequestTimeout}
	}
	ctx := oidc.ClientContext(context.Background(), client)
	return &gcpEnrollmentVerifier{issuer: issuer, keySet: oidc.NewRemoteKeySet(ctx, jwksURL), timeout: cloudEnrollmentRequestTimeout}
}

func (v *gcpEnrollmentVerifier) Verify(ctx context.Context, proof CloudEnrollmentProof, policy store.AgentEnrollmentPolicy) (CloudEnrollmentIdentity, error) {
	cfg, constraints, err := store.DecodeCloudPolicy[store.GCPPolicyConfig, store.GCPSubjectConstraints](policy)
	if err != nil || cfg.Audience == "" {
		return CloudEnrollmentIdentity{}, fmt.Errorf("%w: policy decode", ErrCloudEnrollmentRejected)
	}
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	verifier := oidc.NewVerifier(v.issuer, v.keySet, &oidc.Config{ClientID: cfg.Audience})
	token, err := verifyOIDCToken(ctx, verifier, proof.BearerToken)
	if err != nil {
		return CloudEnrollmentIdentity{}, err
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Google        struct {
			ComputeEngine struct {
				ProjectID  string `json:"project_id"`
				InstanceID string `json:"instance_id"`
			} `json:"compute_engine"`
		} `json:"google"`
	}
	if err := token.Claims(&claims); err != nil {
		return CloudEnrollmentIdentity{}, fmt.Errorf("%w: claims: %v", ErrCloudEnrollmentRejected, err)
	}
	ce := claims.Google.ComputeEngine
	if !claims.EmailVerified || claims.Email == "" || ce.ProjectID == "" || ce.InstanceID == "" {
		return CloudEnrollmentIdentity{}, fmt.Errorf("%w: missing or unverified claims", ErrCloudEnrollmentRejected)
	}
	if !contains(constraints.ProjectIDs, ce.ProjectID) || !contains(constraints.ServiceAccounts, claims.Email) {
		return CloudEnrollmentIdentity{}, fmt.Errorf("%w: policy subject (project %q, service account %q)", ErrCloudEnrollmentRejected, ce.ProjectID, claims.Email)
	}
	return CloudEnrollmentIdentity{AgentID: "gcp:" + ce.ProjectID + ":" + ce.InstanceID}, nil
}
