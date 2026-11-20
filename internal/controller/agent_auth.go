package controller

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/agentauth"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/go-chi/chi/v5"
)

type agentPrincipalContextKey struct{}

// AgentPrincipal identifies the agent authenticated for an agent API request.
type AgentPrincipal struct {
	IdentityID             string
	AgentID                string
	CredentialID           string
	AuthMethod             string
	AuthorizedLabels       []string
	AuthorizedCapabilities []string
}

func withAgentPrincipal(r *http.Request, principal AgentPrincipal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), agentPrincipalContextKey{}, principal))
}

// agentPrincipalFromContext returns the authenticated agent, if any.
func agentPrincipalFromContext(ctx context.Context) (AgentPrincipal, bool) {
	principal, ok := ctx.Value(agentPrincipalContextKey{}).(AgentPrincipal)
	return principal, ok
}

// agentAuth authenticates an opaque enrolled agent credential (uca_ access
// token). Any other bearer is rejected with 401.
func (s *Server) agentAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, status, msg := s.authenticateAgentBearer(r)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		next.ServeHTTP(w, withAgentPrincipal(r, principal))
	})
}

// authenticateAgentBearer authenticates the request's uca_ access token. On
// failure it returns a non-zero HTTP status (401, or 503 when the credential
// store is unavailable) and the message to send; the auth metrics and failure
// report are already recorded.
func (s *Server) authenticateAgentBearer(r *http.Request) (AgentPrincipal, int, string) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, bearerPrefix) {
		s.recordAgentAuth("access", "failure", "invalid")
		reportAuthFailure(nil, r, "agent-access", "missing_bearer")
		return AgentPrincipal{}, http.StatusUnauthorized, "unauthorized"
	}
	token := strings.TrimPrefix(header, bearerPrefix)
	if !strings.HasPrefix(token, "uca_") {
		s.recordAgentAuth("access", "failure", "invalid")
		reportAuthFailure(nil, r, "agent-access", "unsupported_token")
		return AgentPrincipal{}, http.StatusUnauthorized, "unauthorized"
	}
	parsed, err := agentauth.Parse(token, agentauth.AccessToken)
	if err != nil {
		s.recordAgentAuth("access", "failure", "invalid")
		reportAuthFailure(nil, r, "agent-access", "malformed_token")
		return AgentPrincipal{}, http.StatusUnauthorized, "unauthorized"
	}
	if s.store == nil {
		s.recordAgentAuth("access", "failure", "unavailable")
		return AgentPrincipal{}, http.StatusServiceUnavailable, "authentication unavailable"
	}

	credential, err := s.store.GetAgentCredentialForAuth(r.Context(), parsed.ID)
	if err != nil {
		if errors.Is(err, store.ErrAgentCredentialNotFound) || errors.Is(err, store.ErrAgentIdentityDisabled) {
			s.recordAgentAuth("access", "failure", "invalid")
			reportAuthFailure(nil, r, "agent-access", "unknown_credential")
			return AgentPrincipal{}, http.StatusUnauthorized, "unauthorized"
		}
		s.recordAgentAuth("access", "failure", "unavailable")
		return AgentPrincipal{}, http.StatusServiceUnavailable, "authentication unavailable"
	}
	if credential == nil || credential.Kind != "access" || credential.Status != "active" || credential.RevokedAt != nil || !credential.ExpiresAt.After(time.Now()) || !agentauth.Matches(token, credential.TokenHash) {
		s.recordAgentAuth("access", "failure", "invalid")
		reportAuthFailure(nil, r, "agent-access", "invalid_credential")
		return AgentPrincipal{}, http.StatusUnauthorized, "unauthorized"
	}

	principal := AgentPrincipal{
		IdentityID:             credential.IdentityID,
		AgentID:                credential.AgentID,
		CredentialID:           credential.CredentialID,
		AuthMethod:             "bearer",
		AuthorizedLabels:       append([]string(nil), credential.AuthorizedLabels...),
		AuthorizedCapabilities: append([]string(nil), credential.AuthorizedCapabilities...),
	}
	s.recordAgentAuth("access", "success", "ok")
	if s.credentialTouches == nil || s.credentialTouches.shouldTouch(credential.CredentialID) {
		if err := s.store.TouchAgentCredential(r.Context(), credential.CredentialID); err != nil {
			slog.Warn("agent credential touch failed", "credentialID", credential.CredentialID, "error", err)
		}
	}
	return principal, 0, ""
}

// requireAgentPathIdentity prevents an authenticated agent from selecting a
// different agent identity through a route parameter.
func (s *Server) requireAgentPathIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := agentPrincipalFromContext(r.Context())
		if !ok {
			// agentOrServerAuth has already verified a human principal. Only
			// agent principals need a route-identity binding here.
			next.ServeHTTP(w, r)
			return
		}
		if principal.AgentID != chi.URLParam(r, "agentId") {
			s.recordAgentAuth("access", "failure", "policy")
			http.Error(w, "agent identity mismatch", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// agentOrServerAuth allows agent principals to read artifacts while retaining
// existing human PAT, OIDC, and session authentication for those endpoints.
func (s *Server) agentOrServerAuth(next http.Handler) http.Handler {
	serverAuth := ServerAuth(s.store, s)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), bearerPrefix)
		if strings.HasPrefix(token, "uca_") {
			s.agentAuth(next).ServeHTTP(w, r)
			return
		}
		serverAuth(next).ServeHTTP(w, r)
	})
}
