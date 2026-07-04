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
	cfg := &oauth2.Config{
		ClientID:     s.oidcCfg.ClientID,
		ClientSecret: s.oidcCfg.ClientSecret,
		RedirectURL:  redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		Endpoint:     provider.Endpoint(),
	}
	return ctx, provider, cfg, nil
}

// verifyOIDCBearer verifies a Bearer token as a Dex-issued id_token.
// Used to accept id_tokens obtained via the CLI device flow for API authentication.
// The provider is lazily initialized and cached (JWKS is internally cached by go-oidc).
// Prefers the CLI public client (DeviceClientID) as the audience.
func (s *Server) verifyOIDCBearer(ctx context.Context, rawToken string) (*oidc.IDToken, error) {
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
	if s.oidcProviderVErr != nil {
		return nil, s.oidcProviderVErr
	}
	audience := s.oidcCfg.DeviceClientID
	if audience == "" {
		audience = s.oidcCfg.ClientID
	}
	verifier := s.oidcProviderV.Verifier(&oidc.Config{ClientID: audience})
	return verifier.Verify(ctx, rawToken)
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
	verifier := provider.Verifier(&oidc.Config{ClientID: s.oidcCfg.ClientID})
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		http.Error(w, "id_token verification failed: "+err.Error(), http.StatusUnauthorized)
		return
	}

	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "claims parse error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if claims.Email == "" {
		claims.Email = claims.Sub
	}

	if s.km == nil {
		http.Error(w, "key manager not configured", http.StatusInternalServerError)
		return
	}
	encryptedRT, err := s.km.EncryptKey(r.Context(), []byte(oauthToken.RefreshToken))
	if err != nil {
		http.Error(w, "refresh token encryption error", http.StatusInternalServerError)
		return
	}

	sessionToken, err := generatePAT()
	if err != nil {
		http.Error(w, "session token generation error", http.StatusInternalServerError)
		return
	}
	tokenHash := HashToken(sessionToken)
	expiresAt := time.Now().Add(sessionDuration)

	if _, err := s.store.CreateSession(r.Context(), tokenHash, claims.Sub, claims.Email, "admin", hex.EncodeToString(encryptedRT), expiresAt); err != nil {
		http.Error(w, "session store error", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionToken,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, savedState.RedirectTo, http.StatusFound)
}

// handleLogout deletes the session and clears the Cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		hash := HashToken(cookie.Value)
		if sess, _ := s.store.GetSessionByHash(r.Context(), hash); sess != nil {
			_ = s.store.DeleteSession(r.Context(), sess.ID)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleMe returns information about the currently logged-in user. Returns 401 when unauthenticated.
// Supports both Bearer token (PAT) and session cookie auth.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	// 1. Bearer token (PAT)
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, bearerPrefix) {
		token := strings.TrimPrefix(h, bearerPrefix)
		hash := HashToken(token)
		pat, err := s.store.GetPATByHash(r.Context(), hash)
		if err == nil && pat != nil {
			writeJSON(w, http.StatusOK, map[string]string{
				"sub":  pat.Name,
				"name": pat.Name,
			})
			return
		}
	}

	// 2. Session cookie (browser SSO)
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	hash := HashToken(cookie.Value)
	sess, err := s.store.GetSessionByHash(r.Context(), hash)
	if err != nil || sess == nil || sess.ExpiresAt.Before(time.Now()) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"sub":   sess.Sub,
		"email": sess.Email,
	})
}

// refreshSession extends the session expiry using the refresh token.
func (s *Server) refreshSession(ctx context.Context, sess *store.Session, host string) error {
	encryptedRT, err := hex.DecodeString(sess.RefreshToken)
	if err != nil {
		return fmt.Errorf("decode refresh token: %w", err)
	}
	rtBytes, err := s.km.DecryptKey(ctx, encryptedRT)
	if err != nil {
		return fmt.Errorf("decrypt refresh token: %w", err)
	}

	ctx, _, oauth2Cfg, err := s.oidcProvider(ctx, host)
	if err != nil {
		return fmt.Errorf("oidc provider: %w", err)
	}
	newToken, err := oauth2Cfg.TokenSource(ctx, &oauth2.Token{
		RefreshToken: string(rtBytes),
	}).Token()
	if err != nil {
		return fmt.Errorf("token refresh: %w", err)
	}

	newEncrypted, err := s.km.EncryptKey(ctx, []byte(newToken.RefreshToken))
	if err != nil {
		return fmt.Errorf("re-encrypt refresh token: %w", err)
	}
	return s.store.UpdateSessionExpiry(ctx, sess.ID, hex.EncodeToString(newEncrypted), time.Now().Add(sessionDuration))
}
