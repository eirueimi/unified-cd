package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eirueimi/unified-cd/internal/api"
	"golang.org/x/oauth2"
)

// The CLI relay lets the CLI log in through a confidential IdP client without
// ever holding its secret: the controller adds the client credentials server
// side and forwards one upstream request per call. Responses carry only the
// relayed tokens or an OAuth error code and description; raw upstream bodies,
// request URLs, secrets and tokens never reach a response or a log line.

const (
	cliRelayMaxBody     = 64 << 10
	cliRelayMaxUpstream = 1 << 20
	cliRelayTimeout     = 15 * time.Second
	cliRelayMaxErrDesc  = 512
)

// cliRelayLimiters holds one per-IP limiter for each relay route.
type cliRelayLimiters struct {
	deviceStart     *enrollmentLimiter
	deviceToken     *enrollmentLimiter
	refresh         *enrollmentLimiter
	revoke          *enrollmentLimiter
	browserExchange *enrollmentLimiter
}

func newCLIRelayLimiters() *cliRelayLimiters {
	return &cliRelayLimiters{
		deviceStart:     newRateLimiter(nil, time.Second, 20),
		deviceToken:     newRateLimiter(nil, 100*time.Millisecond, 50),
		refresh:         newRateLimiter(nil, 200*time.Millisecond, 50),
		revoke:          newRateLimiter(nil, time.Second, 20),
		browserExchange: newRateLimiter(nil, time.Second, 20),
	}
}

// relayClient is the IdP client a relay call acts as.
type relayClient struct {
	id, secret string
}

func (s *Server) deviceRelayClient() relayClient {
	id := s.oidcCfg.DeviceClientID
	if id == "" {
		id = s.oidcCfg.ClientID
	}
	return relayClient{id: id, secret: s.oidcCfg.DeviceClientSecret}
}

// relayClientFor resolves the client named in a refresh or revoke request. ok
// is false when the relay is not available for that client.
func (s *Server) relayClientFor(name string) (c relayClient, valid, ok bool) {
	switch name {
	case "device":
		c = s.deviceRelayClient()
		return c, true, c.secret != ""
	case "browser":
		c = relayClient{id: s.oidcCfg.ClientID, secret: s.oidcCfg.ClientSecret}
		return c, true, c.secret != ""
	}
	return relayClient{}, false, false
}

// relayAllow applies the route limiter and writes the 429 itself when it is
// exhausted.
func (s *Server) relayAllow(w http.ResponseWriter, r *http.Request, l *enrollmentLimiter, route string) bool {
	if l == nil || l.allow(r, route, "") {
		return true
	}
	w.Header().Set("Retry-After", "1")
	writeJSON(w, http.StatusTooManyRequests, api.CLIOAuthError{Error: "rate_limited", ErrorDescription: "too many requests"})
	return false
}

func relayDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, cliRelayMaxBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, api.CLIOAuthError{Error: "invalid_request", ErrorDescription: "malformed request body"})
		return false
	}
	return true
}

// upstreamResult is one upstream POST: the status and the decoded JSON body.
type upstreamResult struct {
	status int
	body   map[string]any
}

// relayPostForm POSTs form to endpoint with the provider HTTP client carried by
// ctx (so IssuerInternal rewriting applies). Redirects are not followed, so the
// credentials are only ever sent to the discovered endpoint. Transport errors
// are reduced to a fixed message: they embed the request URL.
func relayPostForm(ctx context.Context, endpoint string, form url.Values) (*upstreamResult, error) {
	ctx, cancel := context.WithTimeout(ctx, cliRelayTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errors.New("invalid upstream endpoint")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	base := &http.Client{}
	if c, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok && c != nil {
		cp := *c
		base = &cp
	}
	base.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := base.Do(req)
	if err != nil {
		return nil, errors.New("identity provider request failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cliRelayMaxUpstream))
	if err != nil {
		return nil, errors.New("identity provider response unreadable")
	}
	res := &upstreamResult{status: resp.StatusCode}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &res.body) // a non-JSON body leaves body nil
	}
	return res, nil
}

func str(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

func num(m map[string]any, k string) int {
	if v, ok := m[k].(float64); ok {
		return int(v)
	}
	return 0
}

// relayUpstreamError writes the OAuth error from an upstream failure: only the
// error and error_description fields. The description is dropped entirely when
// it contains any sensitive value of the request (client secret, refresh token,
// device code), raw or form-encoded. An error code outside [a-z_] or a failure
// that carries no OAuth error becomes a generic 502.
func relayUpstreamError(w http.ResponseWriter, res *upstreamResult, what string, sensitive ...string) {
	code := str(res.body, "error")
	if !validOAuthErrorCode(code) {
		slog.Warn("cli relay: unexpected identity provider response", "what", what, "status", res.status)
		writeJSON(w, http.StatusBadGateway, api.CLIOAuthError{Error: "server_error", ErrorDescription: "unexpected response from identity provider"})
		return
	}
	desc := str(res.body, "error_description")
	for _, v := range sensitive {
		if v == "" {
			continue
		}
		if strings.Contains(desc, v) || strings.Contains(desc, url.QueryEscape(v)) {
			desc = ""
			break
		}
	}
	desc = truncateUTF8(desc, cliRelayMaxErrDesc)
	slog.Info("cli relay: identity provider returned an error", "what", what, "status", res.status, "error", code)
	writeJSON(w, http.StatusBadRequest, api.CLIOAuthError{Error: code, ErrorDescription: desc})
}

// validOAuthErrorCode accepts 1-64 characters of [a-z_].
func validOAuthErrorCode(code string) bool {
	if code == "" || len(code) > 64 {
		return false
	}
	for _, r := range code {
		if (r < 'a' || r > 'z') && r != '_' {
			return false
		}
	}
	return true
}

// truncateUTF8 cuts s to at most max bytes on a rune boundary.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// relayTokens turns a successful upstream token response into the CLI response:
// the id_token is verified against the audience allow-list and Expiry is its
// exp. fallbackRefresh is echoed when the IdP does not rotate the refresh token.
func (s *Server) relayTokens(ctx context.Context, w http.ResponseWriter, res *upstreamResult, fallbackRefresh, noIDTokenMsg string) {
	rawID := str(res.body, "id_token")
	if rawID == "" {
		writeJSON(w, http.StatusBadRequest, api.CLIOAuthError{Error: "invalid_grant", ErrorDescription: noIDTokenMsg})
		return
	}
	claims, err := s.verifyBearerClaims(ctx, rawID)
	if err != nil {
		slog.Warn("cli relay: id_token verification failed")
		writeJSON(w, http.StatusBadGateway, api.CLIOAuthError{Error: "server_error", ErrorDescription: "identity provider returned an id_token that failed verification"})
		return
	}
	expiry, ok := idTokenExpiry(claims)
	if !ok {
		writeJSON(w, http.StatusBadGateway, api.CLIOAuthError{Error: "server_error", ErrorDescription: "identity provider returned an id_token without exp"})
		return
	}
	refresh := str(res.body, "refresh_token")
	if refresh == "" {
		refresh = fallbackRefresh
	}
	writeJSON(w, http.StatusOK, api.CLITokenResponse{IDToken: rawID, RefreshToken: refresh, Expiry: expiry})
}

// handleCLIDeviceStart starts a device authorization as the device client.
func (s *Server) handleCLIDeviceStart(w http.ResponseWriter, r *http.Request) {
	if !s.relayAllow(w, r, s.cliRelay.deviceStart, "cli-device-start") {
		return
	}
	if s.oidcCfg == nil || s.oidcCfg.DeviceClientSecret == "" {
		http.NotFound(w, r)
		return
	}
	ctx, _, cfg, err := s.oidcProvider(r.Context(), r.Host)
	if err != nil || cfg.Endpoint.DeviceAuthURL == "" {
		slog.Warn("cli relay: device authorization endpoint unavailable")
		writeJSON(w, http.StatusBadGateway, api.CLIOAuthError{Error: "server_error", ErrorDescription: "identity provider unavailable"})
		return
	}
	c := s.deviceRelayClient()
	form := url.Values{
		"client_id":     {c.id},
		"client_secret": {c.secret},
		"scope":         {strings.Join(oidcScopes(s.oidcCfg.RolesClaim, s.oidcCfg.Scopes), " ")},
	}
	res, err := relayPostForm(ctx, cfg.Endpoint.DeviceAuthURL, form)
	if err != nil {
		slog.Warn("cli relay: device authorization request failed", "reason", err.Error())
		writeJSON(w, http.StatusBadGateway, api.CLIOAuthError{Error: "server_error", ErrorDescription: err.Error()})
		return
	}
	if res.status != http.StatusOK {
		relayUpstreamError(w, res, "device_start", c.secret)
		return
	}
	verification := str(res.body, "verification_uri")
	if verification == "" {
		verification = str(res.body, "verification_url")
	}
	if str(res.body, "device_code") == "" || str(res.body, "user_code") == "" || verification == "" {
		slog.Warn("cli relay: incomplete device authorization response")
		writeJSON(w, http.StatusBadGateway, api.CLIOAuthError{Error: "server_error", ErrorDescription: "unexpected response from identity provider"})
		return
	}
	writeJSON(w, http.StatusOK, api.CLIDeviceStartResponse{
		DeviceCode:              str(res.body, "device_code"),
		UserCode:                str(res.body, "user_code"),
		VerificationURI:         verification,
		VerificationURIComplete: str(res.body, "verification_uri_complete"),
		ExpiresIn:               num(res.body, "expires_in"),
		Interval:                num(res.body, "interval"),
	})
}

// handleCLIDeviceToken makes exactly one upstream token request for a device
// code and relays the result; polling is the CLI's job.
func (s *Server) handleCLIDeviceToken(w http.ResponseWriter, r *http.Request) {
	if !s.relayAllow(w, r, s.cliRelay.deviceToken, "cli-device-token") {
		return
	}
	if s.oidcCfg == nil || s.oidcCfg.DeviceClientSecret == "" {
		http.NotFound(w, r)
		return
	}
	var req api.CLIDeviceTokenRequest
	if !relayDecode(w, r, &req) {
		return
	}
	if req.DeviceCode == "" {
		writeJSON(w, http.StatusBadRequest, api.CLIOAuthError{Error: "invalid_request", ErrorDescription: "deviceCode is required"})
		return
	}
	ctx, _, cfg, err := s.oidcProvider(r.Context(), r.Host)
	if err != nil {
		slog.Warn("cli relay: provider discovery failed")
		writeJSON(w, http.StatusBadGateway, api.CLIOAuthError{Error: "server_error", ErrorDescription: "identity provider unavailable"})
		return
	}
	c := s.deviceRelayClient()
	form := url.Values{
		"grant_type":    {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code":   {req.DeviceCode},
		"client_id":     {c.id},
		"client_secret": {c.secret},
	}
	res, err := relayPostForm(ctx, cfg.Endpoint.TokenURL, form)
	if err != nil {
		slog.Warn("cli relay: device token request failed", "reason", err.Error())
		writeJSON(w, http.StatusBadGateway, api.CLIOAuthError{Error: "server_error", ErrorDescription: err.Error()})
		return
	}
	if res.status != http.StatusOK {
		relayUpstreamError(w, res, "device_token", c.secret, req.DeviceCode)
		return
	}
	s.relayTokens(ctx, w, res, "", "identity provider returned no id_token")
}

// handleCLIRefresh exchanges a refresh token as the named client.
func (s *Server) handleCLIRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.relayAllow(w, r, s.cliRelay.refresh, "cli-refresh") {
		return
	}
	if s.oidcCfg == nil {
		http.NotFound(w, r)
		return
	}
	var req api.CLIRefreshRequest
	if !relayDecode(w, r, &req) {
		return
	}
	c, valid, ok := s.relayClientFor(req.Client)
	if !valid || req.RefreshToken == "" {
		writeJSON(w, http.StatusBadRequest, api.CLIOAuthError{Error: "invalid_request", ErrorDescription: `client must be "device" or "browser" and refreshToken is required`})
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, _, cfg, err := s.oidcProvider(r.Context(), r.Host)
	if err != nil {
		slog.Warn("cli relay: provider discovery failed")
		writeJSON(w, http.StatusBadGateway, api.CLIOAuthError{Error: "server_error", ErrorDescription: "identity provider unavailable"})
		return
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {req.RefreshToken},
		"client_id":     {c.id},
		"client_secret": {c.secret},
	}
	res, err := relayPostForm(ctx, cfg.Endpoint.TokenURL, form)
	if err != nil {
		slog.Warn("cli relay: refresh request failed", "reason", err.Error())
		writeJSON(w, http.StatusBadGateway, api.CLIOAuthError{Error: "server_error", ErrorDescription: err.Error()})
		return
	}
	if res.status != http.StatusOK {
		relayUpstreamError(w, res, "refresh", c.secret, req.RefreshToken)
		return
	}
	s.relayTokens(ctx, w, res, req.RefreshToken, "identity provider returned no id_token on refresh")
}

// handleCLIRevoke revokes a refresh token at the IdP's revocation endpoint.
func (s *Server) handleCLIRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.relayAllow(w, r, s.cliRelay.revoke, "cli-revoke") {
		return
	}
	if s.oidcCfg == nil {
		http.NotFound(w, r)
		return
	}
	var req api.CLIRefreshRequest
	if !relayDecode(w, r, &req) {
		return
	}
	c, valid, ok := s.relayClientFor(req.Client)
	if !valid || req.RefreshToken == "" {
		writeJSON(w, http.StatusBadRequest, api.CLIOAuthError{Error: "invalid_request", ErrorDescription: `client must be "device" or "browser" and refreshToken is required`})
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, provider, _, err := s.oidcProvider(r.Context(), r.Host)
	if err != nil {
		slog.Warn("cli relay: provider discovery failed")
		writeJSON(w, http.StatusBadGateway, api.CLIOAuthError{Error: "server_error", ErrorDescription: "identity provider unavailable"})
		return
	}
	var disc struct {
		RevocationEndpoint string `json:"revocation_endpoint"`
	}
	if provider.Claims(&disc) != nil || disc.RevocationEndpoint == "" {
		http.NotFound(w, r)
		return
	}
	form := url.Values{
		"token":           {req.RefreshToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {c.id},
		"client_secret":   {c.secret},
	}
	res, err := relayPostForm(ctx, disc.RevocationEndpoint, form)
	if err != nil {
		slog.Warn("cli relay: revocation request failed", "reason", err.Error())
		writeJSON(w, http.StatusBadGateway, api.CLIOAuthError{Error: "server_error", ErrorDescription: err.Error()})
		return
	}
	if res.status < 200 || res.status > 299 {
		relayUpstreamError(w, res, "revoke", c.secret, req.RefreshToken)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
