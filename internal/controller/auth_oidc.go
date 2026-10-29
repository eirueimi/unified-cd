package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/eirueimi/unified-cd/internal/store"
	"golang.org/x/oauth2"
)

// hostRewriteTransport rewrites HTTP requests inside the master container from
// the external issuer URL (e.g. http://localhost:5556) to the internal Docker
// service URL (e.g. http://dex:5556). Redirect URLs sent to the browser are not
// modified (server-side only).
type hostRewriteTransport struct {
	from string
	to   string
	next http.RoundTripper // Uses http.DefaultTransport when nil (can be replaced in tests).
}

func (t *hostRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	reqURL := req.URL.String()
	if strings.HasPrefix(reqURL, t.from) {
		newReq := req.Clone(req.Context())
		// Replace the from prefix (e.g. http://localhost:8080/dex) with the to prefix
		// (e.g. http://dex:5556/dex). Both include the issuer path /dex, so paths like
		// /dex/.well-known/... and /dex/token are preserved as-is.
		parsed, err := url.Parse(t.to + reqURL[len(t.from):])
		if err != nil {
			return nil, err
		}
		newReq.URL = parsed
		req = newReq
	}
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(req)
}

const sessionCookieName = "ucd_session"
const sessionDuration = 24 * time.Hour
const stateTTL = 10 * time.Minute
const refreshThreshold = 5 * time.Minute

// sessionCookie builds the ucd_session cookie with the shared attributes.
// maxAge < 0 deletes the cookie (logout); maxAge == 0 omits Max-Age (login,
// which uses Expires instead).
func (s *Server) sessionCookie(value string, expires time.Time, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   !s.cfg.InsecureCookies,
		SameSite: http.SameSiteLaxMode,
	}
}

// generateState generates a random state string for CSRF protection.
func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// oidcProvider builds a provider and oauth2.Config from OIDCConfig.
// When IssuerInternal is set, it injects a hostRewriteTransport into the context so that
// server-side discovery, token exchange, and JWKS fetches all go through the internal URL.
// The auth URL sent to the browser remains the external Issuer URL.
// The returned ctx has the transport injected and can be passed directly to Exchange / Verify.
func (s *Server) oidcProvider(ctx context.Context, host string) (context.Context, *oidc.Provider, *oauth2.Config, error) {
	if s.oidcCfg.IssuerInternal != "" {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{
			Transport: &hostRewriteTransport{
				from: s.oidcCfg.Issuer,
				to:   s.oidcCfg.IssuerInternal,
			},
		})
	}
	provider, err := oidc.NewProvider(ctx, s.oidcCfg.Issuer)
	if err != nil {
		return ctx, nil, nil, fmt.Errorf("OIDC provider discovery: %w", err)
	}
	redirectBase := "http://" + host
	if s.oidcCfg.ExternalURL != "" {
		redirectBase = strings.TrimRight(s.oidcCfg.ExternalURL, "/")
	}
	redirectURL := redirectBase + "/api/v1/auth/oidc-callback"
	scopes := []string{oidc.ScopeOpenID, "email", "profile"}
	// When the role claim is a bare scope-like token (e.g. "groups" for Dex),
	// request it as a scope. Namespaced/`:`-bearing claim names (Auth0/Entra
	// app-roles) are emitted without an extra scope and are skipped here.
	if rc := s.oidcCfg.RolesClaim; rc != "" && !strings.ContainsAny(rc, "/:") {
		scopes = append(scopes, rc)
	}
	cfg := &oauth2.Config{
		ClientID:     s.oidcCfg.ClientID,
		ClientSecret: s.oidcCfg.ClientSecret,
		RedirectURL:  redirectURL,
		Scopes:       scopes,
		Endpoint:     provider.Endpoint(),
	}
	return ctx, provider, cfg, nil
}

// verifierProvider returns the lazily initialised, cached provider used to
// verify id_tokens outside the login redirect flow (JWKS is cached by go-oidc).
func (s *Server) verifierProvider() (*oidc.Provider, error) {
	s.oidcVerifyOnce.Do(func() {
		pctx := context.Background()
		if s.oidcCfg.IssuerInternal != "" {
			pctx = context.WithValue(pctx, oauth2.HTTPClient, &http.Client{
				Transport: &hostRewriteTransport{
					from: s.oidcCfg.Issuer,
					to:   s.oidcCfg.IssuerInternal,
				},
			})
		}
		s.oidcProviderV, s.oidcProviderVErr = oidc.NewProvider(pctx, s.oidcCfg.Issuer)
	})
	return s.oidcProviderV, s.oidcProviderVErr
}

// verifyOIDCBearer verifies a Bearer token as a Dex-issued id_token.
// Used to accept id_tokens obtained via the CLI device flow for API authentication.
// Prefers the CLI public client (DeviceClientID) as the audience.
func (s *Server) verifyOIDCBearer(ctx context.Context, rawToken string) (*oidc.IDToken, error) {
	provider, err := s.verifierProvider()
	if err != nil {
		return nil, err
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: s.bearerAudience()})
	return verifier.Verify(ctx, rawToken)
}

// bearerAudience is the audience CLI id_tokens are issued for.
func (s *Server) bearerAudience() string {
	if s.oidcCfg.DeviceClientID != "" {
		return s.oidcCfg.DeviceClientID
	}
	return s.oidcCfg.ClientID
}

// verifyIDTokenClaims verifies a raw id_token for the given audience and
// returns its claims. The verifyIDTokenHook, when set, replaces the real
// go-oidc verification (used by tests).
func (s *Server) verifyIDTokenClaims(ctx context.Context, raw, audience string) (map[string]any, error) {
	if s.verifyIDTokenHook != nil {
		return s.verifyIDTokenHook(ctx, raw, audience)
	}
	provider, err := s.verifierProvider()
	if err != nil {
		return nil, err
	}
	idToken, err := provider.Verifier(&oidc.Config{ClientID: audience}).Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// verifyBearerClaims verifies a Bearer id_token (CLI device flow) and returns its claims.
func (s *Server) verifyBearerClaims(ctx context.Context, raw string) (map[string]any, error) {
	return s.verifyIDTokenClaims(ctx, raw, s.bearerAudience())
}

// handleOIDCLogin initiates the OIDC Authorization Code Flow.
// Saves the state to the DB and redirects to the IdP's authentication page.
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if s.oidcCfg == nil || s.oidcCfg.ClientSecret == "" {
		http.NotFound(w, r)
		return
	}

	_, _, oauth2Cfg, err := s.oidcProvider(r.Context(), r.Host)
	if err != nil {
		http.Error(w, "OIDC provider error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	state, err := generateState()
	if err != nil {
		http.Error(w, "state generation error", http.StatusInternalServerError)
		return
	}
	expiresAt := time.Now().Add(stateTTL)
	if _, err := s.store.CreateOIDCState(r.Context(), state, "/ui/", expiresAt); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, oauth2Cfg.AuthCodeURL(state), http.StatusFound)
}

// handleOIDCCallback handles the callback from the IdP.
// Validates code+state → exchanges token → verifies ID token → creates session → sets Cookie → redirects to /ui/
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidcCfg == nil || s.oidcCfg.ClientSecret == "" {
		http.NotFound(w, r)
		return
	}

	stateParam := r.URL.Query().Get("state")
	if stateParam == "" {
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}
	savedState, err := s.store.GetAndDeleteOIDCState(r.Context(), stateParam)
	if err != nil || savedState == nil {
		http.Error(w, "invalid or expired state", http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	ctx, provider, oauth2Cfg, err := s.oidcProvider(r.Context(), r.Host)
	if err != nil {
		http.Error(w, "OIDC provider error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	oauthToken, err := oauth2Cfg.Exchange(ctx, code)
	if err != nil {
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadRequest)
		return
	}

	rawIDToken, ok := oauthToken.Extra("id_token").(string)
	if !ok {
		http.Error(w, "missing id_token", http.StatusBadRequest)
		return
	}
	var claims map[string]any
	if s.verifyIDTokenHook != nil {
		claims, err = s.verifyIDTokenHook(ctx, rawIDToken, s.oidcCfg.ClientID)
		if err != nil {
			http.Error(w, "id_token verification failed: "+err.Error(), http.StatusUnauthorized)
			return
		}
	} else {
		verifier := provider.Verifier(&oidc.Config{ClientID: s.oidcCfg.ClientID})
		idToken, err := verifier.Verify(ctx, rawIDToken)
		if err != nil {
			http.Error(w, "id_token verification failed: "+err.Error(), http.StatusUnauthorized)
			return
		}
		if err := idToken.Claims(&claims); err != nil {
			http.Error(w, "claims parse error: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	sessionToken, expiresAt, status, msg := s.completeOIDCLogin(r.Context(), claims, oauthToken.RefreshToken)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}

	http.SetCookie(w, s.sessionCookie(sessionToken, expiresAt, 0))
	http.Redirect(w, r, savedState.RedirectTo, http.StatusFound)
}

// completeOIDCLogin turns verified id_token claims into a stored session. It
// admits the user when the legacy role resolution succeeds or an
// oidc.permissions entry matches, stores the claim values on the session, and
// refreshes the owner snapshot of every PAT the user owns. On failure it
// returns the HTTP status and message to send; status is 0 on success.
func (s *Server) completeOIDCLogin(ctx context.Context, claims map[string]any, refreshToken string) (sessionToken string, expiresAt time.Time, status int, msg string) {
	sub, _ := claims["sub"].(string)
	email, _ := claims["email"].(string)
	if email == "" {
		email = sub
	}
	values := nonNilClaims(extractRoleValues(claims, s.oidcCfg.RolesClaim))
	role, _, admitted := s.identityGrants(values, email, sub)
	if !admitted {
		return "", time.Time{}, http.StatusForbidden, "forbidden: no role assigned"
	}

	if s.km == nil {
		return "", time.Time{}, http.StatusInternalServerError, "key manager not configured"
	}

	sessionToken, err := generatePAT()
	if err != nil {
		return "", time.Time{}, http.StatusInternalServerError, "session token generation error"
	}
	tokenHash := HashToken(sessionToken)
	expiresAt = time.Now().Add(sessionDuration)

	if _, err := s.createSessionWithRefreshToken(ctx, tokenHash, sub, email, role, values, refreshToken, expiresAt); err != nil {
		return "", time.Time{}, http.StatusInternalServerError, "session store error"
	}
	if _, err := s.store.UpdatePATOwnerSnapshot(ctx, sub, email, values); err != nil {
		return "", time.Time{}, http.StatusInternalServerError, "session store error"
	}
	return sessionToken, expiresAt, 0, ""
}

// createSessionWithRefreshToken stores a session with its OIDC refresh token
// envelope-encrypted and bound to the session's own ID.
//
// The binding needs the ID, which Postgres assigns on insert, so the row is
// created with a placeholder and immediately re-encrypted under the real ID.
// This costs one extra UPDATE per login and keeps every stored blob bound.
func (s *Server) createSessionWithRefreshToken(ctx context.Context, tokenHash, sub, email, role string, claims []string, refreshToken string, expiresAt time.Time) (*store.Session, error) {
	// Empty slices, not nil: pgx encodes a nil []byte as SQL NULL, and both
	// refresh-token columns are NOT NULL. `''::bytea` is a valid non-null value.
	sess, err := s.store.CreateSession(ctx, tokenHash, sub, email, role, claims, []byte{}, []byte{}, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	dek, ct, err := secrets.Encrypt(ctx, s.km, []byte(refreshToken), secrets.SessionRefreshBinding(sess.ID))
	if err != nil {
		return nil, fmt.Errorf("encrypt refresh token: %w", err)
	}
	if err := s.store.UpdateSessionExpiry(ctx, sess.ID, dek, ct, expiresAt); err != nil {
		return nil, fmt.Errorf("store refresh token: %w", err)
	}
	sess.RefreshTokenDEK, sess.RefreshTokenCT = dek, ct
	return sess, nil
}

// handleLogout deletes the session and clears the Cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		hash := HashToken(cookie.Value)
		if sess, _ := s.store.GetSessionByHash(r.Context(), hash); sess != nil {
			_ = s.store.DeleteSession(r.Context(), sess.ID)
		}
	}
	http.SetCookie(w, s.sessionCookie("", time.Time{}, -1))
	w.WriteHeader(http.StatusNoContent)
}

// handleMe returns the caller's identity and effective grants. It runs behind
// ServerAuth, so every authenticated principal kind (PAT, OIDC bearer, session)
// is served and an unauthenticated caller gets 401 from the middleware.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	out := map[string]any{
		"name":        p.Name,
		"kind":        p.Kind,
		"role":        p.Role,
		"permissions": p.Grants.View(),
	}
	if p.Sub != "" {
		out["sub"] = p.Sub
	}
	if p.Email != "" {
		out["email"] = p.Email
	}
	writeJSON(w, http.StatusOK, out)
}

// refreshSession extends the session expiry using the refresh token.
//
// When the refreshed id_token verifies and carries the roles claim, the
// session's stored claims are updated and returned with ok=true.
func (s *Server) refreshSession(ctx context.Context, sess *store.Session, host string) ([]string, bool, error) {
	rtBytes, err := secrets.Decrypt(ctx, s.km, sess.RefreshTokenDEK, sess.RefreshTokenCT,
		secrets.SessionRefreshBinding(sess.ID))
	if err != nil {
		logSecretDecryptFailure("oidc-refresh", sess.ID, err)
		return nil, false, fmt.Errorf("decrypt refresh token: %w", err)
	}

	ctx, _, oauth2Cfg, err := s.oidcProvider(ctx, host)
	if err != nil {
		return nil, false, fmt.Errorf("oidc provider: %w", err)
	}
	newToken, err := oauth2Cfg.TokenSource(ctx, &oauth2.Token{
		RefreshToken: string(rtBytes),
	}).Token()
	if err != nil {
		return nil, false, fmt.Errorf("token refresh: %w", err)
	}

	dek, ct, err := secrets.Encrypt(ctx, s.km, []byte(newToken.RefreshToken),
		secrets.SessionRefreshBinding(sess.ID))
	if err != nil {
		return nil, false, fmt.Errorf("re-encrypt refresh token: %w", err)
	}
	if err := s.store.UpdateSessionExpiry(ctx, sess.ID, dek, ct, time.Now().Add(sessionDuration)); err != nil {
		return nil, false, err
	}
	rawIDToken, _ := newToken.Extra("id_token").(string)
	claims, ok := s.refreshedClaims(ctx, rawIDToken)
	if ok {
		if err := s.store.UpdateSessionClaims(ctx, sess.ID, claims); err != nil {
			return nil, false, fmt.Errorf("store session claims: %w", err)
		}
	}
	return claims, ok, nil
}

// refreshedClaims verifies the id_token returned by a refresh (browser client
// audience) and returns its roles-claim values. ok is false when the token is
// absent, unverifiable, or lacks the roles claim, in which case the stored
// claims must be kept.
func (s *Server) refreshedClaims(ctx context.Context, rawIDToken string) ([]string, bool) {
	if rawIDToken == "" {
		return nil, false
	}
	claims, err := s.verifyIDTokenClaims(ctx, rawIDToken, s.oidcCfg.ClientID)
	if err != nil {
		return nil, false
	}
	rolesClaim := s.oidcCfg.RolesClaim
	if rolesClaim == "" {
		rolesClaim = "groups"
	}
	if _, present := claims[rolesClaim]; !present {
		return nil, false
	}
	return nonNilClaims(extractRoleValues(claims, s.oidcCfg.RolesClaim)), true
}
