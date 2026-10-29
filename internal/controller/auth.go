package controller

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
)

const bearerPrefix = "Bearer "

type ctxKey string

const principalCtxKey ctxKey = "principal"

// Principal identifies the authenticated caller.
type Principal struct {
	Name string // PAT name, or OIDC email (fallback to sub)
	Kind string // "pat" | "oidc" | "session"
	// Role is the legacy global role: "admin" | "developer" | "viewer", or ""
	// for a principal admitted only through oidc.permissions grants.
	Role string

	// Sub and Email identify the OIDC user behind the principal (owner PATs,
	// sessions and bearer tokens); empty for role-only PATs and break-glass.
	Sub, Email string
	// Claims are the values of oidc.rolesClaim known for the principal;
	// ClaimsKnown is false when they could not be determined (role-only PATs,
	// pre-upgrade sessions, bearer tokens that omit a claim the config needs).
	Claims      []string
	ClaimsKnown bool

	// Grants are the principal's effective permissions.
	Grants authz.Grants
	// Ceiling is the owner PAT's permission ceiling already applied to Grants
	// (nil = none); CeilingRole and CeilingPerms are the raw pat.Role and
	// pat.Permissions it was derived from, for PAT-from-PAT inheritance.
	Ceiling      *authz.Set
	CeilingRole  string
	CeilingPerms []string
	// OwnerPATID is the PAT row ID when the principal is an owner PAT.
	OwnerPATID string
	// BreakGlass is true for the env:UNIFIED_TOKEN PAT.
	BreakGlass bool
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
//
// Every admitted human principal carries its effective authz.Grants (spec 5.15).
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
						p, ok := srv.patPrincipal(pat)
						if !ok {
							http.Error(w, "forbidden: no role", http.StatusForbidden)
							return
						}
						next.ServeHTTP(w, withPrincipal(r, p))
						return
					}
				}
				// 1b. OIDC id_token (token obtained via the CLI device flow)
				if srv != nil && srv.oidcCfg != nil {
					if claims, err := srv.verifyBearerClaims(r.Context(), token); err == nil {
						email, _ := claims["email"].(string)
						sub, _ := claims["sub"].(string)
						name := email
						if name == "" {
							name = sub
						}
						values := extractRoleValues(claims, srv.oidcCfg.RolesClaim)
						role, grants, admitted := srv.identityGrants(values, email, sub)
						if !admitted {
							http.Error(w, "forbidden: no role", http.StatusForbidden)
							return
						}
						p := Principal{Name: name, Kind: "oidc", Role: role, Sub: sub, Email: email, Grants: grants}
						p.ClaimsKnown = values != nil || !(srv.oidcCfg.Policy.HasGroupKeyedGrants() || len(srv.oidcCfg.RoleMap) > 0)
						if p.ClaimsKnown {
							p.Claims = nonNilClaims(values)
						}
						next.ServeHTTP(w, withPrincipal(r, p))
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
						} else {
							if sess.ExpiresAt.Sub(now) < refreshThreshold && srv.oidcCfg != nil && srv.km != nil {
								// Less than 5 minutes remaining — silent refresh.
								claims, ok, err := srv.refreshSession(r.Context(), sess, r.Host)
								if err != nil {
									_ = st.DeleteSession(context.Background(), sess.ID)
									http.Error(w, "session refresh failed", http.StatusUnauthorized)
									return
								}
								if ok {
									sess.Claims = claims
								}
							}
							go func() { _ = st.TouchSession(context.Background(), sess.ID) }()
							p, ok := srv.sessionPrincipal(sess)
							if !ok {
								http.Error(w, "forbidden: no role", http.StatusForbidden)
								return
							}
							next.ServeHTTP(w, withPrincipal(r, p))
							return
						}
					}
				}
			}

			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
	}
}

// nonNilClaims normalises absent claim values to an empty (non-nil) slice.
func nonNilClaims(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// patPrincipal builds the principal for an authenticated PAT (spec 5.15).
// srv may be nil (ServerAuth(st, nil)); owner PATs then carry no grants.
// ok=false means the PAT is refused with "forbidden: no role".
func (srv *Server) patPrincipal(pat *store.PAT) (Principal, bool) {
	if pat.Name == BootstrapPATName {
		// Break-glass is always admin, checked before OIDC resolution.
		return Principal{Name: pat.Name, Kind: "pat", Role: "admin", BreakGlass: true,
			Grants: authz.AllGrants("break-glass")}, true
	}
	if pat.Owner == nil {
		// Role-only PAT: the same semantics as before, the refusal just moves up front.
		if roleRank(pat.Role) == 0 {
			return Principal{}, false
		}
		return Principal{Name: pat.Name, Kind: "pat", Role: pat.Role,
			Grants: authz.RoleGrants(pat.Role, fmt.Sprintf("role:%s (pat)", pat.Role))}, true
	}
	// Owner PAT: the owner's grants under the current config, capped by the ceiling.
	role, grants, _ := srv.identityGrants(pat.Owner.Claims, pat.Owner.Email, pat.Owner.Sub)
	ceiling := ceilingSet(pat.Role, pat.Permissions)
	if ceiling != nil {
		grants = grants.Restrict(*ceiling, " & pat-ceiling")
		if pat.Permissions != nil {
			role = "" // a permission-list ceiling carries no legacy role
		} else if roleRank(role) > roleRank(pat.Role) {
			role = pat.Role
		}
	}
	return Principal{
		Name: pat.Name, Kind: "pat", Role: role,
		Sub: pat.Owner.Sub, Email: pat.Owner.Email, Claims: nonNilClaims(pat.Owner.Claims), ClaimsKnown: true,
		Grants: grants, Ceiling: ceiling, CeilingRole: pat.Role, CeilingPerms: pat.Permissions,
		OwnerPATID: pat.ID,
	}, true
}

// sessionPrincipal builds the principal for a browser session (spec 5.15).
// ok=false means the session is refused with "forbidden: no role".
func (srv *Server) sessionPrincipal(sess *store.Session) (Principal, bool) {
	name := sess.Email
	if name == "" {
		name = sess.Sub
	}
	p := Principal{Name: name, Kind: "session", Sub: sess.Sub, Email: sess.Email}
	if sess.Claims != nil {
		// Role is recomputed from the stored claims and the current config; the
		// session is admitted even when that leaves no global role.
		p.Role, p.Grants, _ = srv.identityGrants(sess.Claims, sess.Email, sess.Sub)
		p.Claims, p.ClaimsKnown = sess.Claims, true
		return p, true
	}
	// Pre-upgrade session: role-only.
	if roleRank(sess.Role) == 0 {
		return Principal{}, false
	}
	p.Role = sess.Role
	p.Grants = authz.RoleGrants(sess.Role, fmt.Sprintf("role:%s (session)", sess.Role))
	return p, true
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
