package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
)

// refreshSkew is how long before expiry the CLI refreshes the id_token.
const refreshSkew = 60 * time.Second

// refreshTimeout bounds one whole refresh (config fetch, discovery, token
// call). It must stay well under lockStaleAfter, because the config lock is
// held for the duration. A variable so tests can shorten it.
var refreshTimeout = 15 * time.Second

// refreshError is a failed refresh. Reason is safe to print: an OAuth error
// code or a short transport reason, never a token or a response body.
type refreshError struct{ Reason string }

func (e *refreshError) Error() string { return e.Reason }

// refreshWarning is the one-line stderr warning for a failed refresh.
func refreshWarning(err error) string {
	return fmt.Sprintf("token refresh failed: %s; run \"unified-cli login\" again", err.Error())
}

// applyRefresh refreshes c when allowed and prints one warning to stderr when
// the refresh fails (the command then runs with the old token). overridden is
// true when the token came from --token or $UNIFIED_TOKEN, which are never
// refreshed.
func applyRefresh(ctx context.Context, path string, c Config, overridden bool, now time.Time, stderr io.Writer) Config {
	if overridden {
		return c
	}
	got, err := refreshIfNeeded(ctx, path, c, now)
	if err != nil {
		fmt.Fprintln(stderr, refreshWarning(err))
		return c
	}
	return got
}

// idTokenExpiry returns the exp claim of a JWT without verifying it. It is for
// scheduling refreshes only; the controller verifies the token.
func idTokenExpiry(raw string) (time.Time, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&claims); err != nil {
		return time.Time{}, fmt.Errorf("parse JWT claims: %w", err)
	}
	if claims.Exp == "" {
		return time.Time{}, errors.New("JWT has no exp claim")
	}
	f, err := claims.Exp.Float64()
	if err != nil {
		return time.Time{}, fmt.Errorf("parse exp: %w", err)
	}
	return time.Unix(int64(f), 0), nil
}

// saveLogin stores a fresh login in the config file at path. c supplies the
// non-token fields (server, headers); the token fields are set from tok.
func saveLogin(path string, c Config, tok api.CLITokenResponse, client string, relay bool) error {
	c.Token = tok.IDToken
	c.RefreshToken = tok.RefreshToken
	c.TokenExpiry = tok.Expiry
	c.LoginClient = client
	c.LoginRelay = relay
	return SaveConfig(path, c)
}

func tokenStale(c Config, now time.Time) bool {
	return c.RefreshToken != "" && c.TokenExpiry.Before(now.Add(refreshSkew))
}

// refreshIfNeeded refreshes c.Token when a refresh token is stored and the
// id_token expires within 60 s. It takes <path>.lock, re-reads the file (another
// process may have refreshed already), refreshes if still stale, saves the file
// and returns the config with the new token fields. On failure it returns c
// unchanged together with a *refreshError.
func refreshIfNeeded(ctx context.Context, path string, c Config, now time.Time) (Config, error) {
	if !tokenStale(c, now) {
		return c, nil
	}
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return c, &refreshError{Reason: "could not lock the config file"}
	}
	defer unlock()

	stored, err := LoadConfig(path)
	if err != nil {
		return c, &refreshError{Reason: "could not read the config file"}
	}
	if stored.RefreshToken == "" {
		return c, nil
	}
	// The stored refresh token belongs to the stored server only. A --server /
	// $UNIFIED_SERVER override is treated like a token override: no refresh,
	// and nothing is ever sent to the overriding server.
	if strings.TrimRight(c.Server, "/") != strings.TrimRight(stored.Server, "/") || stored.Server == "" {
		return c, nil
	}
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	if !tokenStale(stored, now) {
		// Another process refreshed while we waited for the lock.
		return withTokenFields(c, stored), nil
	}

	var tok api.CLITokenResponse
	if stored.LoginClient == "browser" || stored.LoginRelay {
		client := "device"
		if stored.LoginClient == "browser" {
			client = "browser"
		}
		tok, err = relayRefresh(ctx, stored.Server, stored.RefreshToken, client)
	} else {
		tok, err = directRefresh(ctx, stored.Server, stored.RefreshToken)
	}
	if err != nil {
		return c, err
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = stored.RefreshToken
	}
	// Save the file's config, not c: c carries flag/env overrides.
	updated := stored
	updated.Token = tok.IDToken
	updated.RefreshToken = tok.RefreshToken
	updated.TokenExpiry = tok.Expiry
	if err := SaveConfig(path, updated); err != nil {
		return c, &refreshError{Reason: "could not save the config file"}
	}
	return withTokenFields(c, updated), nil
}

func withTokenFields(c, from Config) Config {
	c.Token = from.Token
	c.RefreshToken = from.RefreshToken
	c.TokenExpiry = from.TokenExpiry
	return c
}

// relayRefresh refreshes through the controller's /api/v1/auth/cli/refresh.
func relayRefresh(ctx context.Context, server, refreshToken, client string) (api.CLITokenResponse, error) {
	var tok api.CLITokenResponse
	body, _ := json.Marshal(api.CLIRefreshRequest{RefreshToken: refreshToken, Client: client})
	if err := postRelay(ctx, server, "/api/v1/auth/cli/refresh", body, &tok); err != nil {
		return tok, err
	}
	if tok.IDToken == "" {
		return tok, &refreshError{Reason: "server returned no id_token"}
	}
	return tok, nil
}

// postRelay POSTs a JSON body to a controller relay endpoint using the shared
// default client (so the extra request headers apply). A 400 is decoded into
// its OAuth error code; other failures get a short reason without any body.
func postRelay(ctx context.Context, server, path string, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(server, "/")+path, bytes.NewReader(body))
	if err != nil {
		return &refreshError{Reason: "invalid server URL"}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &refreshError{Reason: "could not reach the server"}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(b, out); err != nil {
			return &refreshError{Reason: "invalid response from the server"}
		}
		return nil
	case http.StatusBadRequest:
		var oe api.CLIOAuthError
		if json.Unmarshal(b, &oe) == nil && oe.Error != "" {
			return &refreshError{Reason: oauthCode(oe.Error)}
		}
	}
	return &refreshError{Reason: fmt.Sprintf("server returned %d", resp.StatusCode)}
}

// directRefresh refreshes straight against the IdP token endpoint as a public
// client (device client id, no secret). Endpoint and client id come from the
// server's oidc-config, falling back to discovery of its issuer.
func directRefresh(ctx context.Context, server, refreshToken string) (api.CLITokenResponse, error) {
	var tok api.CLITokenResponse
	cfg, err := fetchServerOIDCConfigCtx(ctx, server)
	if err != nil {
		return tok, &refreshError{Reason: "could not fetch the OIDC config from the server"}
	}
	clientID := cfg.DeviceClientID
	if clientID == "" {
		clientID = cfg.ClientID
	}
	tokenURL := cfg.TokenEndpoint
	if tokenURL == "" {
		ep, err := discoverOIDCEndpointCtx(ctx, cfg.Issuer)
		if err != nil || ep.TokenURL == "" {
			return tok, &refreshError{Reason: "could not discover the token endpoint"}
		}
		tokenURL = ep.TokenURL
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tok, &refreshError{Reason: "invalid token endpoint"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return tok, &refreshError{Reason: "could not reach the identity provider"}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr struct {
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
	}
	_ = json.Unmarshal(b, &tr)
	if resp.StatusCode != http.StatusOK {
		if tr.Error != "" {
			return tok, &refreshError{Reason: oauthCode(tr.Error)}
		}
		return tok, &refreshError{Reason: fmt.Sprintf("identity provider returned %d", resp.StatusCode)}
	}
	if tr.IDToken == "" {
		return tok, &refreshError{Reason: "identity provider returned no id_token"}
	}
	exp, err := idTokenExpiry(tr.IDToken)
	if err != nil {
		return tok, &refreshError{Reason: "identity provider returned an unreadable id_token"}
	}
	return api.CLITokenResponse{IDToken: tr.IDToken, RefreshToken: tr.RefreshToken, Expiry: exp}, nil
}

// oauthCode keeps an OAuth error code printable: IdP-supplied text is
// untrusted, so only the characters RFC 6749 allows survive, capped in length.
func oauthCode(code string) string {
	var b strings.Builder
	for _, r := range code {
		if b.Len() >= 64 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown_error"
	}
	return b.String()
}
