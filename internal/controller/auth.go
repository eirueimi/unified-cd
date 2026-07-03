package controller

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
)

const bearerPrefix = "Bearer "

type ctxKey string

const principalCtxKey ctxKey = "principal"

// Principal identifies the authenticated caller.
type Principal struct {
	Name string // PAT name, or OIDC email (fallback to sub)
	Kind string // "pat" | "oidc" | "session"
}

func withPrincipal(r *http.Request, p Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalCtxKey, p))
}

// principalFromContext returns the authenticated principal, if any.
func principalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey).(Principal)
	return p, ok
}

// BootstrapPATName is the fixed identifier used when syncing UNIFIED_TOKEN as a PAT at startup.
// Using this name with UpsertBootstrapPAT / DeleteBootstrapPATByName ensures that when the
// value changes, rows are not duplicated — only the hash is updated.
const BootstrapPATName = "env:UNIFIED_TOKEN"

// BearerAuth returns the existing static-token authentication middleware (for agent auth).
func BearerAuth(expected string) func(http.Handler) http.Handler {
	expectedBytes := []byte(expected)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, bearerPrefix) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			got := []byte(strings.TrimPrefix(h, bearerPrefix))
			if len(expectedBytes) == 0 || subtle.ConstantTimeCompare(got, expectedBytes) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ServerAuth returns an authentication middleware that tries three methods in order:
// PAT, OIDC id_token, and session Cookie. UNIFIED_TOKEN (the static token) is synced to
// the DB as a PAT at startup, so no dedicated comparison branch is needed here
// (it is not special-cased so that deleting it from the UI revokes it immediately).
func ServerAuth(st store.Store, srv *Server) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 1. Bearer token
			h := r.Header.Get("Authorization")
			if strings.HasPrefix(h, bearerPrefix) {
				token := strings.TrimPrefix(h, bearerPrefix)
				// 1a. PAT (including those synced from UNIFIED_TOKEN)
				if st != nil {
					hash := HashToken(token)
					pat, err := st.GetPATByHash(r.Context(), hash)
					if err == nil && pat != nil {
						go func() { _ = st.TouchPAT(context.Background(), pat.ID) }()
						next.ServeHTTP(w, withPrincipal(r, Principal{Name: pat.Name, Kind: "pat"}))
						return
					}
				}
				// 1b. OIDC id_token (token obtained via the CLI device flow)
				if srv != nil && srv.oidcCfg != nil {
					if idToken, err := srv.verifyOIDCBearer(r.Context(), token); err == nil {
						var claims struct {
							Sub   string `json:"sub"`
							Email string `json:"email"`
						}
						_ = idToken.Claims(&claims)
						name := claims.Email
						if name == "" {
							name = claims.Sub
						}
						next.ServeHTTP(w, withPrincipal(r, Principal{Name: name, Kind: "oidc"}))
						return
					}
				}
			}

			// 2. Session Cookie (browser SSO)
			if st != nil && srv != nil {
				if cookie, err := r.Cookie(sessionCookieName); err == nil {
					hash := HashToken(cookie.Value)
					sess, err := st.GetSessionByHash(r.Context(), hash)
					if err == nil && sess != nil {
						now := time.Now()
						if sess.ExpiresAt.Before(now) {
							// Expired — delete the session.
							_ = st.DeleteSession(context.Background(), sess.ID)
						} else if sess.ExpiresAt.Sub(now) < refreshThreshold && srv.oidcCfg != nil && srv.km != nil {
							// Less than 5 minutes remaining — silent refresh.
							if err := srv.refreshSession(r.Context(), sess, r.Host); err != nil {
								_ = st.DeleteSession(context.Background(), sess.ID)
								http.Error(w, "session refresh failed", http.StatusUnauthorized)
								return
							}
							go func() { _ = st.TouchSession(context.Background(), sess.ID) }()
							sessName := sess.Email
							if sessName == "" {
								sessName = sess.Sub
							}
							next.ServeHTTP(w, withPrincipal(r, Principal{Name: sessName, Kind: "session"}))
							return
						} else {
							go func() { _ = st.TouchSession(context.Background(), sess.ID) }()
							sessName := sess.Email
							if sessName == "" {
								sessName = sess.Sub
							}
							next.ServeHTTP(w, withPrincipal(r, Principal{Name: sessName, Kind: "session"}))
							return
						}
					}
				}
			}

			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
	}
}

// AgentOrServerAuth allows the agent static token (constant-time compare) OR,
// failing that, any identity ServerAuth accepts (PAT / OIDC / session). Used for
// artifact download + list, which both agents and humans need.
func AgentOrServerAuth(agentToken string, st store.Store, srv *Server) func(http.Handler) http.Handler {
	server := ServerAuth(st, srv)
	expected := []byte(agentToken)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if strings.HasPrefix(h, bearerPrefix) {
				got := []byte(strings.TrimPrefix(h, bearerPrefix))
				if len(expected) != 0 && subtle.ConstantTimeCompare(got, expected) == 1 {
					next.ServeHTTP(w, r)
					return
				}
			}
			server(next).ServeHTTP(w, r)
		})
	}
}

// HashToken returns the SHA-256 hash of a token string as a hex string.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
