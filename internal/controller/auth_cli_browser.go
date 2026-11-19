package controller

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/eirueimi/unified-cd/internal/store"
)

// Browser login for the CLI. The CLI opens /api/v1/auth/cli/browser/start in a
// browser with a loopback redirect and a PKCE challenge; the normal OIDC
// callback then issues a one-time code instead of a session and sends the
// browser to the loopback. The CLI redeems the code, proving possession of the
// PKCE verifier, at /api/v1/auth/cli/browser/exchange. The tokens never travel
// through the browser: only the code does, and it is useless without the
// verifier, single use, and short lived.

const (
	// cliLoginCodeTTL is how long a one-time code can be redeemed.
	cliLoginCodeTTL = 2 * time.Minute

	errBrowserLoginNeedsKM = "browser login requires the controller key manager"
)

// validateCLIRedirect accepts only the exact loopback callback the CLI listens
// on: http://127.0.0.1:<port>/callback or http://[::1]:<port>/callback. There
// is no userinfo, query or fragment, and the string must be in canonical form,
// so no lookalike host or parser-differential trick survives.
func validateCLIRedirect(raw string) bool {
	if raw == "" || len(raw) > 64 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" {
		return false
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "::1" {
		return false
	}
	port := u.Port()
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return false
	}
	canonical := "http://" + u.Host + "/callback"
	// Comparing against the canonical form rejects an empty query or fragment
	// marker, a trailing slash, dot segments, whitespace and any other residue.
	return raw == canonical
}

// validPKCEChallenge accepts a base64url (unpadded) S256 challenge: 32 bytes.
func validPKCEChallenge(c string) bool {
	if len(c) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	return err == nil && len(b) == sha256.Size
}

func pkceS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// hashCLICode is the stored form of a one-time code: SHA-256 hex.
func hashCLICode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// cliBrowserAvailable writes the 404 or 503 for a controller that cannot serve
// browser login and reports whether it can.
func (s *Server) cliBrowserAvailable(w http.ResponseWriter, r *http.Request) bool {
	if s.oidcCfg == nil || s.oidcCfg.ClientSecret == "" {
		http.NotFound(w, r)
		return false
	}
	if s.km == nil {
		http.Error(w, errBrowserLoginNeedsKM, http.StatusServiceUnavailable)
		return false
	}
	return true
}

// handleCLIBrowserStart begins a CLI browser login and redirects to the IdP.
func (s *Server) handleCLIBrowserStart(w http.ResponseWriter, r *http.Request) {
	if !s.cliBrowserAvailable(w, r) {
		return
	}
	redirect := r.URL.Query().Get("redirect")
	challenge := r.URL.Query().Get("challenge")
	if !validateCLIRedirect(redirect) {
		http.Error(w, "redirect must be http://127.0.0.1:<port>/callback or http://[::1]:<port>/callback", http.StatusBadRequest)
		return
	}
	if !validPKCEChallenge(challenge) {
		http.Error(w, "challenge must be a base64url SHA-256 PKCE challenge", http.StatusBadRequest)
		return
	}
	_, _, oauth2Cfg, err := s.oidcProvider(r.Context(), r.Host)
	if err != nil {
		slog.Warn("cli browser login: provider discovery failed")
		http.Error(w, "OIDC provider error", http.StatusInternalServerError)
		return
	}
	state, err := generateState()
	if err != nil {
		http.Error(w, "state generation error", http.StatusInternalServerError)
		return
	}
	if _, err := s.store.CreateCLIOIDCState(r.Context(), state, redirect, challenge, time.Now().Add(stateTTL)); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, oauth2Cfg.AuthCodeURL(state, s.authCodeOptions()...), http.StatusFound)
}

// cliLoginPayload is the encrypted content of a one-time code.
type cliLoginPayload struct {
	IDToken      string `json:"idToken"`
	RefreshToken string `json:"refreshToken"`
}

// completeCLIBrowserLogin finishes the OIDC callback of a CLI browser login:
// it applies the same admission as the web login, stores the tokens encrypted
// under a fresh one-time code and redirects the browser to the loopback with
// the code. No session is created.
func (s *Server) completeCLIBrowserLogin(w http.ResponseWriter, r *http.Request, st *store.OIDCState, claims map[string]any, refreshToken, rawIDToken string) {
	if status, msg := s.admitOIDCClaims(r.Context(), claims); status != 0 {
		http.Error(w, msg, status)
		return
	}
	if s.km == nil {
		http.Error(w, errBrowserLoginNeedsKM, http.StatusServiceUnavailable)
		return
	}
	// The redirect was validated at start; check again so a tampered row can
	// never turn the callback into an open redirect.
	if !validateCLIRedirect(st.CLIRedirect) || !validPKCEChallenge(st.CLIChallenge) {
		http.Error(w, "invalid login state", http.StatusBadRequest)
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, "code generation error", http.StatusInternalServerError)
		return
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	payload, err := json.Marshal(cliLoginPayload{IDToken: rawIDToken, RefreshToken: refreshToken})
	if err != nil {
		http.Error(w, "login error", http.StatusInternalServerError)
		return
	}
	// The ciphertext is bound to the row id, which the database assigns on
	// insert: create the row with an empty payload, then fill it in. The code
	// has not left the controller yet, so nothing can redeem the empty row.
	expiresAt := time.Now().Add(cliLoginCodeTTL)
	id, err := s.store.CreateCLILoginCode(r.Context(), hashCLICode(code), st.CLIChallenge, []byte{}, []byte{}, expiresAt)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	dek, ct, err := secrets.Encrypt(r.Context(), s.km, payload, secrets.CLILoginCodeBinding(id))
	if err != nil {
		http.Error(w, "login error", http.StatusInternalServerError)
		return
	}
	if err := s.store.SetCLILoginCodePayload(r.Context(), id, dek, ct); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Location", st.CLIRedirect+"?code="+code)
	w.WriteHeader(http.StatusFound)
}

// handleCLIBrowserExchange redeems a one-time code. The code is consumed
// before anything else is checked, so any attempt, including one with a wrong
// verifier, burns it.
func (s *Server) handleCLIBrowserExchange(w http.ResponseWriter, r *http.Request) {
	if !s.relayAllow(w, r, s.cliRelay.browserExchange, "cli-browser-exchange") {
		return
	}
	if !s.cliBrowserAvailable(w, r) {
		return
	}
	var req api.CLIBrowserExchangeRequest
	if !relayDecode(w, r, &req) {
		return
	}
	if req.Code == "" || req.CodeVerifier == "" {
		writeJSON(w, http.StatusBadRequest, api.CLIOAuthError{Error: "invalid_request", ErrorDescription: "code and codeVerifier are required"})
		return
	}
	invalid := func() {
		writeJSON(w, http.StatusBadRequest, api.CLIOAuthError{Error: "invalid_grant"})
	}
	row, err := s.store.ConsumeCLILoginCode(r.Context(), hashCLICode(req.Code))
	if err != nil {
		slog.Warn("cli browser login: code lookup failed")
		writeJSON(w, http.StatusInternalServerError, api.CLIOAuthError{Error: "server_error", ErrorDescription: "store error"})
		return
	}
	if row == nil {
		invalid()
		return
	}
	if subtle.ConstantTimeCompare([]byte(pkceS256(req.CodeVerifier)), []byte(row.Challenge)) != 1 {
		invalid()
		return
	}
	plain, err := secrets.Decrypt(r.Context(), s.km, row.DEK, row.CT, secrets.CLILoginCodeBinding(row.ID))
	if err != nil {
		logSecretDecryptFailure("cli-browser-exchange", row.ID, err)
		writeJSON(w, http.StatusInternalServerError, api.CLIOAuthError{Error: "server_error", ErrorDescription: "login payload unreadable"})
		return
	}
	var payload cliLoginPayload
	if err := json.Unmarshal(plain, &payload); err != nil || payload.IDToken == "" {
		writeJSON(w, http.StatusInternalServerError, api.CLIOAuthError{Error: "server_error", ErrorDescription: "login payload unreadable"})
		return
	}
	claims, err := s.verifyBearerClaims(r.Context(), payload.IDToken)
	if err != nil {
		slog.Warn("cli browser login: id_token verification failed")
		writeJSON(w, http.StatusBadGateway, api.CLIOAuthError{Error: "server_error", ErrorDescription: "id_token failed verification"})
		return
	}
	expiry, ok := idTokenExpiry(claims)
	if !ok {
		writeJSON(w, http.StatusBadGateway, api.CLIOAuthError{Error: "server_error", ErrorDescription: "id_token has no exp"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, api.CLITokenResponse{IDToken: payload.IDToken, RefreshToken: payload.RefreshToken, Expiry: expiry})
}

// idTokenExpiry returns the id_token exp claim as a UTC time.
func idTokenExpiry(claims map[string]any) (time.Time, bool) {
	exp, ok := claims["exp"].(float64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(exp), 0).UTC(), true
}
