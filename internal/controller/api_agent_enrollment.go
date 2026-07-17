package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/agentauth"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/go-chi/chi/v5"
)

const defaultAgentEnrollmentTTL = 10 * time.Minute

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
		if err != nil || parsed <= 0 {
			http.Error(w, "expiresIn must be a positive duration", http.StatusBadRequest)
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
