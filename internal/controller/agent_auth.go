package controller

import (
	"context"
	"crypto/subtle"
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

// agentAuth authenticates an opaque agent credential or the explicit legacy
// shared token. Invalid opaque credentials never fall back to legacy auth.
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

		legacy := []byte(s.cfg.LegacyAgentToken)
		if len(legacy) == 0 || subtle.ConstantTimeCompare([]byte(token), legacy) != 1 {
			s.recordAgentAuth("access", "failure", "invalid")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if s.metrics != nil {
			s.metrics.AgentLegacyAuth()
		}
		next.ServeHTTP(w, withAgentPrincipal(r, AgentPrincipal{AuthMethod: "legacy"}))
	})
}

// requireAgentPathIdentity prevents an authenticated agent from selecting a
// different agent identity through a route parameter. The legacy shared-token
// migration mode deliberately preserves its existing, unbound behavior.
func (s *Server) requireAgentPathIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := agentPrincipalFromContext(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if principal.AuthMethod != "legacy" && principal.AgentID != chi.URLParam(r, "agentId") {
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
		if strings.HasPrefix(token, "uca_") || (s.cfg.LegacyAgentToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.LegacyAgentToken)) == 1) {
			s.agentAuth(next).ServeHTTP(w, r)
			return
		}
		serverAuth(next).ServeHTTP(w, r)
	})
}
