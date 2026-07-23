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
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, bearerPrefix) {
			s.recordAgentAuth("access", "failure", "invalid")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(header, bearerPrefix)
		if strings.HasPrefix(token, "uca_") {
			parsed, err := agentauth.Parse(token, agentauth.AccessToken)
			if err != nil || s.store == nil {
				if err != nil {
					s.recordAgentAuth("access", "failure", "invalid")
					http.Error(w, "unauthorized", http.StatusUnauthorized)
				} else {
					s.recordAgentAuth("access", "failure", "unavailable")
					http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
				}
				return
			}

			credential, err := s.store.GetAgentCredentialForAuth(r.Context(), parsed.ID)
			if err != nil {
				if errors.Is(err, store.ErrAgentCredentialNotFound) || errors.Is(err, store.ErrAgentIdentityDisabled) {
					s.recordAgentAuth("access", "failure", "invalid")
					http.Error(w, "unauthorized", http.StatusUnauthorized)
				} else {
					s.recordAgentAuth("access", "failure", "unavailable")
					http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
				}
				return
			}
			if credential == nil || credential.Kind != "access" || credential.Status != "active" || credential.RevokedAt != nil || !credential.ExpiresAt.After(time.Now()) || !agentauth.Matches(token, credential.TokenHash) {
				s.recordAgentAuth("access", "failure", "invalid")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
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
			next.ServeHTTP(w, withAgentPrincipal(r, principal))
			return
		}

		s.recordAgentAuth("access", "failure", "invalid")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
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

// viewerOrAgent admits an authenticated agent principal, otherwise enforces the
// human viewer role. Pair it with agentOrServerAuth on read routes that both an
// enrolled agent and a human viewer may call.
func (s *Server) viewerOrAgent(next http.Handler) http.Handler {
	viewer := requireMinRole("viewer")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := agentPrincipalFromContext(r.Context()); ok {
			next.ServeHTTP(w, r)
			return
		}
		viewer(next).ServeHTTP(w, r)
	})
}
