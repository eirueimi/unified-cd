package controller

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/jackc/pgx/v5"
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

	// Sub and Email identify the OIDC user behind the principal (human
	// accounts: sessions, bearer tokens and their PATs); empty for service
	// accounts.
	Sub, Email string
	// AccountID is the accounts row the principal authenticated as, and
	// AccountKind/AccountName its kind ("human" | "service") and name (the
	// service account name; the display name for a human).
	AccountID, AccountKind, AccountName string
	// Claims are the values of oidc.rolesClaim known for the principal;
	// ClaimsKnown is false when they could not be determined (bearer tokens
	// that omit a claim the config needs).
	Claims      []string
	ClaimsKnown bool

	// Grants are the principal's effective permissions.
	Grants authz.Grants
	// Ceiling is the PAT's permission ceiling already applied to Grants
	// (nil = none); CeilingRole and CeilingPerms are the raw pat.Role and
	// pat.Permissions it was derived from, for PAT-from-PAT inheritance.
	Ceiling      *authz.Set
	CeilingRole  string
	CeilingPerms []string
	// OwnerPATID is the PAT row ID when the principal authenticated with a PAT
	// (every PAT belongs to an account); empty otherwise.
	OwnerPATID string
	// PATID is the PAT row ID for every PAT-authenticated principal (Kind
	// "pat"), owner or not; empty otherwise. Unlike Name it is unique.
	PATID string
	// EnrollLabels bounds the labels an agent.enroll PAT may put on the
	// enrollment tokens it creates; nil means no bound.
	EnrollLabels []string
}

// AuditActorID returns the stable, unambiguous identity recorded in the audit
// log next to the display Name (which is a user-chosen PAT name or an email and
// is not unique): "pat:<id>" for a PAT, "oidc:<sub>" for an OIDC bearer token
// or session, and "" when nothing stable is known.
func (p Principal) AuditActorID() string {
	switch {
	case p.Kind == "pat" && p.PATID != "":
		return "pat:" + p.PATID
	case p.Sub != "" && (p.Kind == "oidc" || p.Kind == "session"):
		return "oidc:" + p.Sub
	}
	return ""
}

func withPrincipal(r *http.Request, p Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalCtxKey, p))
}

// principalFromContext returns the authenticated principal, if any.
func principalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey).(Principal)
	return p, ok
}

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

// msgBrowserPATRefused is the 401 body for a PAT presented by a browser while
// SSO is configured and ui.allowTokenEntry is off.
const msgBrowserPATRefused = "personal access tokens are not accepted from the browser while SSO is configured; sign in with SSO"

// ServerAuth returns an authentication middleware that tries three methods in order:
// PAT, OIDC id_token, and session Cookie. Every credential resolves to an
// account; a disabled or unknown account is refused with 403.
//
// Every admitted principal carries its effective authz.Grants (spec 5.15).
func ServerAuth(st store.Store, srv *Server) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			lookupFailed := false
			// 1. Bearer token
			h := r.Header.Get("Authorization")
			if strings.HasPrefix(h, bearerPrefix) {
				token := strings.TrimPrefix(h, bearerPrefix)
				// 1a. PAT: always owned by an account.
				if st != nil {
					hash := HashToken(token)
					pat, err := st.GetPATByHash(r.Context(), hash)
					if err != nil && !errors.Is(err, pgx.ErrNoRows) {
						lookupFailed = true
					}
					if err == nil && pat != nil {
						// With SSO configured, a PAT must not be typed into the
						// browser. Browsers send Sec-Fetch-Site on every request
						// to a secure context; Node's fetch sends Sec-Fetch-Mode
						// but not Sec-Fetch-Site, so it is not mistaken for one.
						if srv != nil && !srv.allowBrowserTokenEntry() && r.Header.Get("Sec-Fetch-Site") != "" {
							http.Error(w, msgBrowserPATRefused, http.StatusUnauthorized)
							return
						}
						go func() { _ = st.TouchPAT(context.Background(), pat.ID) }()
						p, msg := srv.patPrincipal(r.Context(), pat)
						if msg != "" {
							http.Error(w, msg, http.StatusForbidden)
							return
						}
						next.ServeHTTP(w, withPrincipal(r, p))
						return
					}
				}
				// 1b. OIDC id_token (token obtained via the CLI device flow)
				if srv != nil && srv.oidcCfg != nil {
					if claims, err := srv.verifyBearerClaims(r.Context(), token); err == nil {
						email := verifiedEmail(claims)
						sub, _ := claims["sub"].(string)
						name := email
						if name == "" {
							name = sub
						}
						values := extractRoleValues(claims, srv.oidcCfg.RolesClaim)
						// Bearer requests evaluate claims every time, so the
						// overage warning is rate-limited per subject.
						srv.warnGroupsOverage(claims, values, true)
						role, grants, admitted := srv.identityGrants(values, email, sub)
						if !admitted {
							http.Error(w, msgNoRole, http.StatusForbidden)
							return
						}
						// Admitted: record the human account (at most once a
						// minute per sub) and refuse a disabled one.
						acct, aerr := srv.idTokenAccount(r.Context(), sub, email, nonNilClaims(values))
						if aerr != nil {
							slog.Error("upsert human account failed", "error", aerr)
							http.Error(w, "account store error", http.StatusInternalServerError)
							return
						}
						if acct.DisabledAt != nil {
							http.Error(w, msgAccountDisabled, http.StatusForbidden)
							return
						}
						p := Principal{Name: name, Kind: "oidc", Role: role, Sub: sub, Email: email, Grants: grants,
							AccountID: acct.ID, AccountKind: store.AccountKindHuman, AccountName: name}
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
						} else if acct := srv.sessionAccount(r.Context(), sess); acct == nil || acct.DisabledAt != nil {
							// Checked before the refresh so a disabled account
							// never reaches the IdP.
							http.Error(w, msgAccountDisabled, http.StatusForbidden)
							return
						} else {
							if sess.ExpiresAt.Sub(now) < refreshThreshold && srv.oidcCfg != nil && srv.km != nil {
								// Less than 5 minutes remaining — silent refresh.
								claims, ok, err := srv.refreshSession(r.Context(), sess, r.Host)
								if err != nil {
									_ = st.DeleteSession(context.Background(), sess.ID)
									reportAuthFailure(st, r, "session", "refresh_failed")
									http.Error(w, "session refresh failed", http.StatusUnauthorized)
									return
								}
								if ok {
									sess.Claims = claims
								}
							}
							go func() { _ = st.TouchSession(context.Background(), sess.ID) }()
							p, msg := srv.sessionPrincipal(r.Context(), sess, acct)
							if msg != "" {
								http.Error(w, msg, http.StatusForbidden)
								return
							}
							next.ServeHTTP(w, withPrincipal(r, p))
							return
						}
					}
				}
			}

			method, reason := "none", "missing_credentials"
			if strings.HasPrefix(h, bearerPrefix) {
				method, reason = "bearer", "invalid_token"
				if lookupFailed {
					reason = "lookup_failed"
				}
			} else if _, err := r.Cookie(sessionCookieName); err == nil {
				method, reason = "session", "invalid_or_expired_session"
			}
			var aw auditWriter
			if st != nil {
				aw = st
			}
			reportAuthFailure(aw, r, method, reason)
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

// patPrincipal builds the principal for an authenticated PAT: the owning
// account's grants under the current config, capped by the PAT's ceiling. A
// non-empty msg means the PAT is refused with 403 and that body. A PAT without
// an account (a row from before accounts existed) is refused.
func (srv *Server) patPrincipal(ctx context.Context, pat *store.PAT) (Principal, string) {
	if srv == nil || srv.store == nil || pat.AccountID == "" {
		return Principal{}, msgAccountDisabled
	}
	acct, err := srv.store.GetAccount(ctx, pat.AccountID)
	if err != nil {
		slog.Error("load account for PAT failed", "error", err)
		return Principal{}, msgAccountDisabled
	}
	role, grants, msg := srv.resolveAccount(ctx, acct)
	if msg != "" {
		return Principal{}, msg
	}
	ceiling := ceilingSet(pat.Role, pat.Permissions)
	if ceiling != nil && ceiling.Has(authz.AgentEnroll) && grants.Can(authz.AgentManage, "") {
		// agent.manage covers agent.enroll for the owner, so an enrollment PAT
		// minted by such an owner keeps working.
		var extra authz.Grants
		extra.Add("", authz.Set(0).With(authz.AgentEnroll), "agent.manage (implies agent.enroll)")
		grants = grants.Union(extra)
	}
	if ceiling != nil {
		grants = grants.Restrict(*ceiling, " & pat-ceiling")
		if pat.Permissions != nil {
			role = "" // a permission-list ceiling carries no legacy role
		} else if roleRank(role) > roleRank(pat.Role) {
			role = pat.Role
		}
	}
	p := Principal{
		Name: pat.Name, Kind: "pat", Role: role,
		AccountID: acct.ID, AccountKind: acct.Kind, AccountName: acct.Name,
		Grants: grants, Ceiling: ceiling, CeilingRole: pat.Role, CeilingPerms: pat.Permissions,
		OwnerPATID: pat.ID, PATID: pat.ID, EnrollLabels: pat.EnrollLabels,
		ClaimsKnown: true, Claims: nonNilClaims(acct.Claims),
	}
	if acct.Kind == store.AccountKindHuman {
		p.Sub, p.Email = acct.OIDCSub, acct.Email
		p.AccountName = acct.Email
		if p.AccountName == "" {
			p.AccountName = acct.OIDCSub
		}
	}
	return p, ""
}

// sessionAccount loads the account a session belongs to; nil when the session
// has none or the lookup fails.
func (srv *Server) sessionAccount(ctx context.Context, sess *store.Session) *store.Account {
	if srv == nil || srv.store == nil || sess.AccountID == "" {
		return nil
	}
	acct, err := srv.store.GetAccount(ctx, sess.AccountID)
	if err != nil {
		slog.Error("load account for session failed", "error", err)
		return nil
	}
	return acct
}

// sessionPrincipal builds the principal for a browser session. The grants come
// from the session's account; the session's own claims (refreshed from the
// IdP) take precedence over the account snapshot. A non-empty msg means the
// session is refused with 403 and that body.
func (srv *Server) sessionPrincipal(ctx context.Context, sess *store.Session, acct *store.Account) (Principal, string) {
	name := sess.Email
	if name == "" {
		name = sess.Sub
	}
	view := *acct
	if sess.Claims != nil {
		view.Claims = sess.Claims
	}
	role, grants, msg := srv.resolveAccount(ctx, &view)
	if msg != "" {
		return Principal{}, msg
	}
	return Principal{
		Name: name, Kind: "session", Role: role, Sub: sess.Sub, Email: sess.Email,
		AccountID: acct.ID, AccountKind: acct.Kind, AccountName: name,
		Claims: nonNilClaims(view.Claims), ClaimsKnown: true, Grants: grants,
	}, ""
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
