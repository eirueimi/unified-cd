package controller

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
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

// oidcScopes returns the scopes both the browser login and the CLI device flow
// request. When override (oidc.scopes) is non-empty it is used as-is, with
// openid prepended if missing and duplicates dropped; the rolesClaim scope is
// then not added. Otherwise, when the roles claim is a bare scope-like token
// (e.g. "groups" for Dex) it is requested as a scope too, so the IdP emits it.
// Namespaced or `:`-bearing claim names (Auth0/Entra app-roles) are emitted
// without an extra scope and are skipped.
func oidcScopes(rolesClaim string, override []string) []string {
	if len(override) > 0 {
		scopes := []string{oidc.ScopeOpenID}
		seen := map[string]bool{oidc.ScopeOpenID: true}
		for _, sc := range override {
			if !seen[sc] {
				seen[sc] = true
				scopes = append(scopes, sc)
			}
		}
		return scopes
	}
	scopes := []string{oidc.ScopeOpenID, "email", "profile"}
	if rolesClaim != "" && !strings.ContainsAny(rolesClaim, "/:.") {
		scopes = append(scopes, rolesClaim)
	}
	return scopes
}

// oidcProvider builds a provider and oauth2.Config from OIDCConfig.
// When IssuerInternal is set, it injects a hostRewriteTransport into the context so that
// server-side discovery, token exchange, and JWKS fetches all go through the internal URL.
// The auth URL sent to the browser remains the external Issuer URL.
// The returned ctx has the transport injected and can be passed directly to Exchange / Verify.
func (s *Server) oidcProvider(ctx context.Context, host string) (context.Context, *oidc.Provider, *oauth2.Config, error) {
	ctx = s.oidcHTTPContext(ctx)
	provider, err := oidc.NewProvider(ctx, s.oidcCfg.Issuer)
	if err != nil {
		return ctx, nil, nil, fmt.Errorf("OIDC provider discovery: %w", err)
	}
	return ctx, provider, s.oauth2Config(provider, host), nil
}

// oidcHTTPContext injects the IssuerInternal rewriting HTTP client into ctx
// when one is configured.
func (s *Server) oidcHTTPContext(ctx context.Context) context.Context {
	if s.oidcCfg.IssuerInternal != "" {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{
			Transport: &hostRewriteTransport{
				from: s.oidcCfg.Issuer,
				to:   s.oidcCfg.IssuerInternal,
			},
		})
	}
	return ctx
}

// oauth2Config builds the per-request OAuth client config for a discovered
// provider. The redirect URL depends on the request host, so it is never cached.
func (s *Server) oauth2Config(provider *oidc.Provider, host string) *oauth2.Config {
	redirectBase := "http://" + host
	if s.oidcCfg.ExternalURL != "" {
		redirectBase = strings.TrimRight(s.oidcCfg.ExternalURL, "/")
	}
	redirectURL := redirectBase + "/api/v1/auth/oidc-callback"
	scopes := oidcScopes(s.oidcCfg.RolesClaim, s.oidcCfg.Scopes)
	return &oauth2.Config{
		ClientID:     s.oidcCfg.ClientID,
		ClientSecret: s.oidcCfg.ClientSecret,
		RedirectURL:  redirectURL,
		Scopes:       scopes,
		Endpoint:     provider.Endpoint(),
	}
}

// relayDiscoveryTTL is how long a successful IdP discovery is reused by the
// CLI relay and the CLI browser-login endpoints; relayDiscoveryTimeout bounds
// one discovery request.
const (
	relayDiscoveryTTL     = 10 * time.Minute
	relayDiscoveryTimeout = 10 * time.Second
)

// discoveryFlight is one in-progress discovery shared by all waiting callers.
type discoveryFlight struct {
	key      string
	done     chan struct{}
	provider *oidc.Provider
	err      error
}

// relayDiscoveryCache holds the last successful discovery. It is keyed by the
// issuer pair, so a config swap never serves a stale provider. Failures are
// never stored. Cache hits take only a read lock; the lock is never held
// during I/O. The zero value is ready to use; now and timeout are replaceable
// in tests.
type relayDiscoveryCache struct {
	mu       sync.RWMutex
	now      func() time.Time
	timeout  time.Duration
	key      string
	provider *oidc.Provider
	at       time.Time
	flight   *discoveryFlight
}

func (c *relayDiscoveryCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// fresh returns the cached provider for key when it is still valid. The caller
// holds mu.
func (c *relayDiscoveryCache) fresh(key string) *oidc.Provider {
	if c.provider != nil && c.key == key && c.clock().Sub(c.at) < relayDiscoveryTTL {
		return c.provider
	}
	return nil
}

// get returns the cached provider or runs discover once for all concurrent
// callers. The shared discovery runs on its own context (see discover), so a
// caller that gives up only stops waiting: it returns ctx.Err() and the
// discovery carries on for the others.
func (c *relayDiscoveryCache) get(ctx context.Context, key string, discover func(context.Context) (*oidc.Provider, error)) (*oidc.Provider, error) {
	c.mu.RLock()
	p := c.fresh(key)
	c.mu.RUnlock()
	if p != nil {
		return p, nil
	}

	c.mu.Lock()
	if p := c.fresh(key); p != nil {
		c.mu.Unlock()
		return p, nil
	}
	f := c.flight
	if f == nil || f.key != key {
		f = &discoveryFlight{key: key, done: make(chan struct{})}
		c.flight = f
		timeout := c.timeout
		if timeout <= 0 {
			timeout = relayDiscoveryTimeout
		}
		go func() {
			dctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			provider, err := func() (p *oidc.Provider, err error) {
				// A panic in discover must not leave the flight open: every
				// waiter would block until its own context ended.
				defer func() {
					if r := recover(); r != nil {
						p, err = nil, fmt.Errorf("OIDC discovery panic: %v", r)
					}
				}()
				return discover(dctx)
			}()
			c.mu.Lock()
			if err == nil {
				c.provider, c.key, c.at = provider, key, c.clock()
			}
			if c.flight == f {
				c.flight = nil
			}
			c.mu.Unlock()
			f.provider, f.err = provider, err
			close(f.done)
		}()
	}
	c.mu.Unlock()

	select {
	case <-f.done:
		return f.provider, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// relayProvider is oidcProvider backed by a 10-minute cache of successful
// discovery. Concurrent callers share one discovery request, bounded by its own
// 10 s timeout and independent of any caller's request context. The
// oauth2.Config is rebuilt per call because its RedirectURL depends on host.
func (s *Server) relayProvider(ctx context.Context, host string) (context.Context, *oidc.Provider, *oauth2.Config, error) {
	ctx = s.oidcHTTPContext(ctx)
	issuer := s.oidcCfg.Issuer
	key := issuer + "\x00" + s.oidcCfg.IssuerInternal
	provider, err := s.relayDisc.get(ctx, key, func(dctx context.Context) (*oidc.Provider, error) {
		return oidc.NewProvider(s.oidcHTTPContext(dctx), issuer)
	})
	if err != nil {
		return ctx, nil, nil, fmt.Errorf("OIDC provider discovery: %w", err)
	}
	return ctx, provider, s.oauth2Config(provider, host), nil
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

// verifyOIDCBearer verifies a Bearer token as an IdP-issued id_token.
// Used to accept id_tokens obtained via the CLI login flows for API authentication.
// The audience must be one of bearerAudiences(); signature, issuer and expiry
// are verified on every attempt (SkipClientIDCheck is never used).
func (s *Server) verifyOIDCBearer(ctx context.Context, rawToken string) (*oidc.IDToken, error) {
	provider, err := s.verifierProvider()
	if err != nil {
		return nil, err
	}
	var firstErr error
	for _, aud := range s.bearerAudiences() {
		idToken, err := provider.Verifier(&oidc.Config{ClientID: aud}).Verify(ctx, rawToken)
		if err == nil {
			return idToken, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// bearerAudiences is the allow-list of audiences CLI id_tokens may carry: the
// device client (when set) followed by the browser client, without duplicates
// or empty entries.
func (s *Server) bearerAudiences() []string {
	var out []string
	for _, aud := range []string{s.oidcCfg.DeviceClientID, s.oidcCfg.ClientID} {
		if aud == "" || slices.Contains(out, aud) {
			continue
		}
		out = append(out, aud)
	}
	return out
}

// authCodeOptions returns the extra authorization-request parameters configured
// through oidc.authParams, in a stable order.
func (s *Server) authCodeOptions() []oauth2.AuthCodeOption {
	keys := make([]string, 0, len(s.oidcCfg.AuthParams))
	for k := range s.oidcCfg.AuthParams {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var opts []oauth2.AuthCodeOption
	for _, k := range keys {
		opts = append(opts, oauth2.SetAuthURLParam(k, s.oidcCfg.AuthParams[k]))
	}
	return opts
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

// verifyBearerClaims verifies a Bearer id_token (CLI login flows) against the
// audience allow-list and returns its claims.
func (s *Server) verifyBearerClaims(ctx context.Context, raw string) (map[string]any, error) {
	var firstErr error
	for _, aud := range s.bearerAudiences() {
		claims, err := s.verifyIDTokenClaims(ctx, raw, aud)
		if err == nil {
			return claims, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
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

	http.Redirect(w, r, oauth2Cfg.AuthCodeURL(state, s.authCodeOptions()...), http.StatusFound)
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

	// A CLI login is only completed in the browser that confirmed it: the
	// callback cookie set by /cli/browser/continue must match the hash stored on
	// the state. This runs before the IdP-error branch and the token exchange so
	// a refused request never reaches the loopback and never spends the IdP code.
	if savedState.CLIRedirect != "" {
		c, cerr := r.Cookie(s.cliCookieName(cliCallbackCookie))
		if cerr != nil || c.Value == "" || savedState.CLICallbackHash == "" ||
			subtle.ConstantTimeCompare([]byte(hashCLICode(c.Value)), []byte(savedState.CLICallbackHash)) != 1 {
			http.SetCookie(w, s.cliCallbackCookieFor("", -1))
			cliPageError(w, errCLIConfirmRequired, http.StatusForbidden)
			return
		}
		http.SetCookie(w, s.cliCallbackCookieFor("", -1))
	}

	// The IdP reported an error (for example access_denied): a CLI login hands
	// it to the loopback so the CLI stops waiting; the web login keeps its page.
	if idpErr := r.URL.Query().Get("error"); idpErr != "" && savedState.CLIRedirect != "" {
		if redirectCLIBrowserError(w, savedState, idpErr) {
			return
		}
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
			reportAuthFailure(s.store, r, "oidc-callback", "id_token_verification_failed")
			http.Error(w, "id_token verification failed: "+err.Error(), http.StatusUnauthorized)
			return
		}
	} else {
		verifier := provider.Verifier(&oidc.Config{ClientID: s.oidcCfg.ClientID})
		idToken, err := verifier.Verify(ctx, rawIDToken)
		if err != nil {
			reportAuthFailure(s.store, r, "oidc-callback", "id_token_verification_failed")
			http.Error(w, "id_token verification failed: "+err.Error(), http.StatusUnauthorized)
			return
		}
		if err := idToken.Claims(&claims); err != nil {
			http.Error(w, "claims parse error: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if savedState.CLIRedirect != "" {
		s.completeCLIBrowserLogin(w, r, savedState, claims, oauthToken.RefreshToken, rawIDToken)
		return
	}

	sessionToken, expiresAt, status, msg := s.completeOIDCLogin(r.Context(), claims, oauthToken.RefreshToken)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}

	http.SetCookie(w, s.sessionCookie(sessionToken, expiresAt, 0))
	http.Redirect(w, r, savedState.RedirectTo, http.StatusFound)
}

// groupsOverageMessage is the warning logged when the IdP replaced the groups
// claim with an overage marker (Entra ID). It is a fixed string so operators can
// grep for it; docs/operator-manual/identity-providers.md quotes it verbatim.
const groupsOverageMessage = `oidc: the identity provider reported a groups overage; group-based roles and permissions are not applied for this login (see identity-providers.md "Entra ID")`

// overageWarnInterval is the minimum gap between two overage warnings for the
// same subject on the bearer path, which evaluates claims on every request.
const overageWarnInterval = 10 * time.Minute

// verifiedEmail returns the id_token e-mail, or "" when it is absent or the IdP
// reports it as unverified (email_verified is the JSON boolean false or the
// string "false"). Any other type of email_verified counts as absent. Every
// path that matches on e-mail must use this instead of reading claims["email"].
func verifiedEmail(claims map[string]any) string {
	email, _ := claims["email"].(string)
	switch v := claims["email_verified"].(type) {
	case bool:
		if !v {
			return ""
		}
	case string:
		if strings.EqualFold(v, "false") {
			return ""
		}
	}
	return email
}

// hasGroupsOverage reports whether the claims carry an Entra ID overage marker
// for the configured roles claim: its name (default "groups") as a key of
// _claim_names, or, only when the roles claim is "groups", hasgroups set to true.
func hasGroupsOverage(claims map[string]any, rolesClaim string) bool {
	if rolesClaim == "" {
		rolesClaim = "groups"
	}
	if names, ok := claims["_claim_names"].(map[string]any); ok {
		if _, ok := names[rolesClaim]; ok {
			return true
		}
	}
	if rolesClaim != "groups" {
		return false
	}
	switch v := claims["hasgroups"].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	}
	return false
}

// warnGroupsOverage logs groupsOverageMessage when the token has no roles-claim
// values but carries an overage marker. It logs only the subject and the claim
// name, never the e-mail or any token. With limited set, a subject is warned
// about at most once per overageWarnInterval.
func (s *Server) warnGroupsOverage(claims map[string]any, values []string, limited bool) {
	if len(values) > 0 || !hasGroupsOverage(claims, s.oidcCfg.RolesClaim) {
		return
	}
	sub, _ := claims["sub"].(string)
	if limited && !s.overageWarn.allow(sub, time.Now()) {
		return
	}
	rolesClaim := s.oidcCfg.RolesClaim
	if rolesClaim == "" {
		rolesClaim = "groups"
	}
	slog.Warn(groupsOverageMessage, "sub", sub, "rolesClaim", rolesClaim)
}

// overageLimiterMax bounds the number of subjects the limiter remembers.
const overageLimiterMax = 4096

// overageLimiter allows one event per key per overageWarnInterval.
type overageLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (l *overageLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.last[key]; ok && now.Sub(t) < overageWarnInterval {
		return false
	}
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	if len(l.last) >= overageLimiterMax { // drop expired entries first
		for k, t := range l.last {
			if now.Sub(t) >= overageWarnInterval {
				delete(l.last, k)
			}
		}
	}
	// Strictly bounded: when the map is still full of live entries, new subjects
	// are not recorded. They are allowed (logged) on every call instead, so no
	// warning is ever lost; only the rate limit is relaxed under that load.
	if len(l.last) < overageLimiterMax {
		l.last[key] = now
	}
	return true
}

// oidcIdentity extracts the subject, e-mail (falling back to the subject) and
// roles-claim values from verified id_token claims. An e-mail the IdP reports as
// unverified is treated as empty, so matching falls back to the subject. A
// groups overage is logged here (once per call).
func oidcIdentity(s *Server, claims map[string]any) (sub, email string, values []string) {
	sub, email, values = oidcIdentityQuiet(s, claims)
	s.warnGroupsOverage(claims, values, false)
	return sub, email, values
}

// oidcIdentityQuiet is oidcIdentity without the overage warning, for callers
// that already evaluated the same claims.
func oidcIdentityQuiet(s *Server, claims map[string]any) (sub, email string, values []string) {
	sub, _ = claims["sub"].(string)
	email = verifiedEmail(claims)
	if email == "" {
		email = sub
	}
	return sub, email, nonNilClaims(extractRoleValues(claims, s.oidcCfg.RolesClaim))
}

// admitOIDCClaims decides whether verified id_token claims may log in, for both
// the web login and the CLI browser login. The admission check runs first; only
// an admitted identity is written to the accounts table (a refused person
// leaves no row), and a disabled account is refused. On failure it returns the
// HTTP status and message to send; status is 0 when the user is admitted.
func (s *Server) admitOIDCClaims(ctx context.Context, claims map[string]any) (status int, msg string) {
	_, status, msg = s.admitOIDCAccount(ctx, claims)
	return status, msg
}

// admitOIDCAccount is admitOIDCClaims that also returns the human account of
// an admitted login.
func (s *Server) admitOIDCAccount(ctx context.Context, claims map[string]any) (acct *store.Account, status int, msg string) {
	sub, email, values := oidcIdentity(s, claims)
	if _, _, admitted := s.identityGrants(values, email, sub); !admitted {
		return nil, http.StatusForbidden, "forbidden: no role assigned"
	}
	acct, err := s.upsertHuman(ctx, sub, email, values)
	if err != nil {
		return nil, http.StatusInternalServerError, "session store error"
	}
	if acct.DisabledAt != nil {
		return nil, http.StatusForbidden, msgAccountDisabled
	}
	return acct, 0, ""
}

// completeOIDCLogin turns verified id_token claims into a stored session. It
// admits the user when the legacy role resolution succeeds or an
// oidc.permissions entry matches, stores the claim values on the session, and
// records the account. On failure it
// returns the HTTP status and message to send; status is 0 on success.
func (s *Server) completeOIDCLogin(ctx context.Context, claims map[string]any, refreshToken string) (sessionToken string, expiresAt time.Time, status int, msg string) {
	// Admission (and the account upsert) runs before the session exists, so a
	// failure leaves no orphaned session row.
	acct, status, msg := s.admitOIDCAccount(ctx, claims)
	if status != 0 {
		return "", time.Time{}, status, msg
	}
	// admitOIDCClaims already evaluated (and warned about) these claims.
	sub, email, values := oidcIdentityQuiet(s, claims)
	role, _, _ := s.identityGrants(values, email, sub)

	if s.km == nil {
		return "", time.Time{}, http.StatusInternalServerError, "key manager not configured"
	}

	sessionToken, err := generatePAT()
	if err != nil {
		return "", time.Time{}, http.StatusInternalServerError, "session token generation error"
	}
	tokenHash := HashToken(sessionToken)
	expiresAt = time.Now().Add(sessionDuration)

	if _, err := s.createSessionWithRefreshToken(ctx, acct.ID, tokenHash, sub, email, role, values, refreshToken, expiresAt); err != nil {
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
func (s *Server) createSessionWithRefreshToken(ctx context.Context, accountID, tokenHash, sub, email, role string, claims []string, refreshToken string, expiresAt time.Time) (*store.Session, error) {
	// Empty slices, not nil: pgx encodes a nil []byte as SQL NULL, and both
	// refresh-token columns are NOT NULL. `''::bytea` is a valid non-null value.
	sess, err := s.store.CreateAccountSession(ctx, accountID, tokenHash, sub, email, role, claims, []byte{}, []byte{}, expiresAt)
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
	values := extractRoleValues(claims, s.oidcCfg.RolesClaim)
	s.warnGroupsOverage(claims, values, false)
	if _, present := lookupClaim(claims, s.oidcCfg.RolesClaim); !present {
		return nil, false
	}
	return nonNilClaims(values), true
}
