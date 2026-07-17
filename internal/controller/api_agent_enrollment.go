package controller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/agentauth"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	agentAccessTTL      = time.Hour
	agentRefreshTTL     = 30 * 24 * time.Hour
	agentRefreshOverlap = 5 * time.Minute
)

const (
	defaultAgentEnrollmentTTL = 10 * time.Minute
	maxAgentEnrollmentTTL     = 24 * time.Hour
)

// handleCreateAgentEnrollment creates an opaque, one-time enrollment token.
// The plaintext token is returned only in this creation response.
func (s *Server) handleCreateAgentEnrollment(w http.ResponseWriter, r *http.Request) {
	var req api.CreateAgentEnrollmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.AgentID = strings.TrimSpace(req.AgentID)
	if req.AgentID == "" {
		http.Error(w, "agentId is required", http.StatusBadRequest)
		return
	}

	ttl := defaultAgentEnrollmentTTL
	if req.ExpiresIn != "" {
		parsed, err := time.ParseDuration(req.ExpiresIn)
		if err != nil || parsed <= 0 || parsed > maxAgentEnrollmentTTL {
			http.Error(w, "expiresIn must be a positive duration no greater than 24h", http.StatusBadRequest)
			return
		}
		ttl = parsed
	}
	for _, capability := range req.Capabilities {
		if !dsl.ValidCapability(capability) {
			http.Error(w, "unknown capability: "+capability, http.StatusBadRequest)
			return
		}
	}

	issued, err := agentauth.Generate(agentauth.EnrollmentToken)
	if err != nil {
		http.Error(w, "generate enrollment token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	creator, _ := principalFromContext(r.Context())
	created, err := s.store.CreateAgentEnrollmentToken(r.Context(), store.AgentEnrollmentToken{
		ID: issued.ID, AgentID: req.AgentID, CreatedBy: creator.Name,
		AuthorizedLabels: req.Labels, AuthorizedCapabilities: req.Capabilities,
		ExpiresAt: time.Now().Add(ttl),
	}, issued.Hash)
	if err != nil {
		http.Error(w, "create enrollment token: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, api.CreateAgentEnrollmentResponse{
		ID: created.ID, AgentID: created.AgentID, Token: issued.Plaintext, ExpiresAt: created.ExpiresAt,
	})
}

func (s *Server) handleListAgentEnrollments(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.store.ListAgentEnrollmentTokens(r.Context())
	if err != nil {
		http.Error(w, "list enrollment tokens: "+err.Error(), http.StatusInternalServerError)
		return
	}
	result := make([]api.AgentEnrollmentMeta, 0, len(tokens))
	for _, token := range tokens {
		result = append(result, enrollmentMeta(token))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleRevokeAgentEnrollment(w http.ResponseWriter, r *http.Request) {
	if err := s.store.RevokeAgentEnrollmentToken(r.Context(), chi.URLParam(r, "id")); err != nil {
		http.Error(w, "revoke enrollment token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetAgentIdentity(w http.ResponseWriter, r *http.Request) {
	identity, err := s.store.GetAgentIdentity(r.Context(), chi.URLParam(r, "agentId"))
	if err != nil {
		http.Error(w, "get agent identity: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if identity == nil {
		http.Error(w, "agent identity not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, identityMeta(*identity))
}

func (s *Server) handleEnableAgentIdentity(w http.ResponseWriter, r *http.Request) {
	s.setAgentIdentityEnabled(w, r, true)
}

func (s *Server) handleDisableAgentIdentity(w http.ResponseWriter, r *http.Request) {
	s.setAgentIdentityEnabled(w, r, false)
}

func (s *Server) setAgentIdentityEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	if err := s.store.SetAgentIdentityEnabled(r.Context(), chi.URLParam(r, "agentId"), enabled); err != nil {
		if errors.Is(err, store.ErrAgentCredentialNotFound) {
			http.Error(w, "agent identity not found", http.StatusNotFound)
			return
		}
		http.Error(w, "set agent identity status: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRevokeAgentCredentials(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "agentId")
	identity, err := s.store.GetAgentIdentity(r.Context(), agentID)
	if err != nil {
		http.Error(w, "get agent identity: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if identity == nil {
		http.Error(w, "agent identity not found", http.StatusNotFound)
		return
	}
	if err := s.store.RevokeAgentIdentityCredentials(r.Context(), agentID); err != nil {
		http.Error(w, "revoke agent credentials: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func enrollmentMeta(token store.AgentEnrollmentToken) api.AgentEnrollmentMeta {
	return api.AgentEnrollmentMeta{
		ID: token.ID, AgentID: token.AgentID, CreatedBy: token.CreatedBy, ExpiresAt: token.ExpiresAt,
		CreatedAt: token.CreatedAt, UsedAt: token.UsedAt, RevokedAt: token.RevokedAt,
	}
}

func identityMeta(identity store.AgentIdentity) api.AgentIdentityMeta {
	return api.AgentIdentityMeta{
		ID: identity.ID, AgentID: identity.AgentID, Status: identity.Status, EnrollmentMethod: identity.EnrollmentMethod,
		AuthorizedLabels: identity.AuthorizedLabels, AuthorizedCapabilities: identity.AuthorizedCapabilities,
		CreatedAt: identity.CreatedAt, DisabledAt: identity.DisabledAt, LastAuthenticatedAt: identity.LastAuthenticatedAt,
	}
}

// handleAgentEnroll exchanges a valid one-time enrollment credential for the
// VM's initial short-lived access and refresh credentials.
func (s *Server) handleAgentEnroll(w http.ResponseWriter, r *http.Request) {
	if !s.allowAgentCredentialRequest(w, r, "one-time-token", "agent.enrollment.exchange") {
		return
	}
	var req api.AgentEnrollRequest
	if r.ContentLength > 0 && json.NewDecoder(r.Body).Decode(&req) != nil {
		s.recordAgentAuth("one-time-token", "failure", "invalid")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token, ok := bearerToken(r)
	if !ok {
		s.recordAgentAuth("one-time-token", "failure", "invalid")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	parsed, err := agentauth.Parse(token, agentauth.EnrollmentToken)
	if err != nil {
		s.recordAgentAuth("one-time-token", "failure", "invalid")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.store == nil {
		s.recordAgentAuth("one-time-token", "failure", "unavailable")
		http.Error(w, "enrollment unavailable", http.StatusServiceUnavailable)
		return
	}
	access, refresh, err := issueAgentTokenPair()
	if err != nil {
		s.recordAgentAuth("one-time-token", "failure", "unavailable")
		http.Error(w, "enrollment unavailable", http.StatusServiceUnavailable)
		return
	}
	now := time.Now()
	familyID := uuid.NewString()
	identity, err := s.store.ConsumeAgentEnrollment(r.Context(), parsed.ID, agentauth.Hash(token), store.AgentCredentialIssue{
		EnrollmentMethod: "enrollment", Access: newAgentCredential(access, "access", familyID, 1, now.Add(agentAccessTTL)),
		Refresh: ptrAgentCredential(newAgentCredential(refresh, "refresh", familyID, 1, now.Add(agentRefreshTTL))),
	})
	if err != nil {
		s.respondAgentCredentialError(r, w, "one-time-token", "agent.enrollment.exchange", parsed.ID, err)
		return
	}
	s.recordAgentAuth("one-time-token", "success", "ok")
	s.auditAgentCredential(r, "agent.enrollment.exchange", identity.AgentID, http.StatusOK)
	writeJSON(w, http.StatusOK, agentTokenResponse(identity, access.Plaintext, refresh.Plaintext, now))
}

// handleAgentRefresh rotates a VM refresh credential. Access credentials are
// deliberately rejected here and no access-token renewal route exists.
func (s *Server) handleAgentRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.allowAgentCredentialRequest(w, r, "refresh", "agent.refresh") {
		return
	}
	token, ok := bearerToken(r)
	if !ok {
		s.recordAgentAuth("refresh", "failure", "invalid")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	parsed, err := agentauth.Parse(token, agentauth.RefreshToken)
	if err != nil {
		s.recordAgentAuth("refresh", "failure", "invalid")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.store == nil {
		s.recordAgentAuth("refresh", "failure", "unavailable")
		http.Error(w, "enrollment unavailable", http.StatusServiceUnavailable)
		return
	}
	access, refresh, err := issueAgentTokenPair()
	if err != nil {
		s.recordAgentAuth("refresh", "failure", "unavailable")
		http.Error(w, "enrollment unavailable", http.StatusServiceUnavailable)
		return
	}
	now := time.Now()
	identity, err := s.store.RotateAgentRefresh(r.Context(), parsed.ID, agentauth.Hash(token), now,
		newAgentCredential(access, "access", "", 0, now.Add(agentAccessTTL)),
		newAgentCredential(refresh, "refresh", "", 0, now.Add(agentRefreshTTL)), agentRefreshOverlap)
	if err != nil {
		s.respondAgentCredentialError(r, w, "refresh", "agent.refresh", parsed.ID, err)
		return
	}
	s.recordAgentAuth("refresh", "success", "ok")
	s.auditAgentCredential(r, "agent.refresh", identity.AgentID, http.StatusOK)
	writeJSON(w, http.StatusOK, agentTokenResponse(identity, access.Plaintext, refresh.Plaintext, now))
}

func (s *Server) allowAgentCredentialRequest(w http.ResponseWriter, r *http.Request, provider, action string) bool {
	// VM providers are server-selected. No caller-controlled body value may
	// create a distinct limiter bucket before provider policy validation.
	if s.enrollmentLimiter != nil && s.enrollmentLimiter.allow(r, provider, "") {
		return true
	}
	w.Header().Set("Retry-After", "6")
	s.recordAgentAuth(provider, "failure", "rate_limited")
	s.auditAgentCredential(r, action, "", http.StatusTooManyRequests)
	http.Error(w, "enrollment rate limit exceeded", http.StatusTooManyRequests)
	return false
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, bearerPrefix) {
		return "", false
	}
	token := strings.TrimPrefix(header, bearerPrefix)
	return token, token != ""
}

func issueAgentTokenPair() (agentauth.IssuedToken, agentauth.IssuedToken, error) {
	access, err := agentauth.Generate(agentauth.AccessToken)
	if err != nil {
		return agentauth.IssuedToken{}, agentauth.IssuedToken{}, err
	}
	refresh, err := agentauth.Generate(agentauth.RefreshToken)
	if err != nil {
		return agentauth.IssuedToken{}, agentauth.IssuedToken{}, err
	}
	return access, refresh, nil
}

func newAgentCredential(token agentauth.IssuedToken, kind, familyID string, generation int, expiresAt time.Time) store.NewAgentCredential {
	return store.NewAgentCredential{ID: token.ID, Kind: kind, FamilyID: familyID, Generation: generation, TokenHash: token.Hash, ExpiresAt: expiresAt}
}

func ptrAgentCredential(credential store.NewAgentCredential) *store.NewAgentCredential {
	return &credential
}

func agentTokenResponse(identity *store.AgentIdentity, accessToken, refreshToken string, now time.Time) api.AgentTokenResponse {
	refreshExpiresAt := now.Add(agentRefreshTTL)
	return api.AgentTokenResponse{AgentID: identity.AgentID, AccessToken: accessToken, AccessExpiresAt: now.Add(agentAccessTTL), RefreshToken: refreshToken,
		RefreshExpiresAt: &refreshExpiresAt, Labels: append([]string(nil), identity.AuthorizedLabels...), Capabilities: append([]string(nil), identity.AuthorizedCapabilities...)}
}

func (s *Server) respondAgentCredentialError(r *http.Request, w http.ResponseWriter, provider, action, resource string, err error) {
	if errors.Is(err, store.ErrAgentIdentityDisabled) {
		s.recordAgentAuth(provider, "failure", "disabled")
		s.auditAgentCredential(r, action, resource, http.StatusForbidden)
		http.Error(w, "agent identity disabled", http.StatusForbidden)
		return
	}
	if errors.Is(err, store.ErrAgentRefreshReplay) {
		s.recordAgentAuth(provider, "failure", "replay")
		s.auditAgentCredential(r, action, resource, http.StatusUnauthorized)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if errors.Is(err, store.ErrAgentEnrollmentInvalid) || errors.Is(err, store.ErrAgentCredentialNotFound) {
		s.recordAgentAuth(provider, "failure", "invalid")
		s.auditAgentCredential(r, action, resource, http.StatusUnauthorized)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.recordAgentAuth(provider, "failure", "unavailable")
	s.auditAgentCredential(r, action, resource, http.StatusServiceUnavailable)
	http.Error(w, "enrollment unavailable", http.StatusServiceUnavailable)
}

func (s *Server) recordAgentAuth(provider, result, reason string) {
	if s.metrics != nil {
		s.metrics.AgentAuthEvent(provider, result, reason)
	}
}

func (s *Server) auditAgentCredential(r *http.Request, action, resource string, status int) {
	if s.store == nil {
		return
	}
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	path := "/api/v1/agents/token"
	if r != nil {
		path = r.URL.Path
	}
	if err := s.store.InsertAuditLog(ctx, "agent", http.MethodPost, path, action, resource, status); err != nil {
		slog.Warn("agent credential audit failed", "action", action, "error", err)
	}
}
