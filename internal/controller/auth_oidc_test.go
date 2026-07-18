package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureRoundTripper is a mock that records the final URL passed to RoundTrip.
type captureRoundTripper struct{ gotURL string }

func (c *captureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.gotURL = req.URL.String()
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

func TestHostRewriteTransport_PreservesIssuerPath(t *testing.T) {
	// Dex uses the issuer path (/dex) as a prefix for all routes.
	// hostRewriteTransport must preserve this path when rewriting the external URL to the internal URL.
	cap := &captureRoundTripper{}
	tr := &hostRewriteTransport{
		from: "http://localhost:8080/dex",
		to:   "http://dex:5556/dex",
		next: cap,
	}

	cases := []struct {
		name string
		url  string
		want string
	}{
		{"discovery", "http://localhost:8080/dex/.well-known/openid-configuration", "http://dex:5556/dex/.well-known/openid-configuration"},
		{"token", "http://localhost:8080/dex/token", "http://dex:5556/dex/token"},
		{"deviceCode", "http://localhost:8080/dex/device/code", "http://dex:5556/dex/device/code"},
		{"jwks", "http://localhost:8080/dex/keys", "http://dex:5556/dex/keys"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, tc.url, nil)
			require.NoError(t, err)
			_, err = tr.RoundTrip(req)
			require.NoError(t, err)
			assert.Equal(t, tc.want, cap.gotURL, "issuer path /dex must be preserved")
		})
	}
}

func TestHostRewriteTransport_NonMatchingURLUnchanged(t *testing.T) {
	// URLs that do not match the from prefix are forwarded unchanged.
	cap := &captureRoundTripper{}
	tr := &hostRewriteTransport{from: "http://localhost:8080/dex", to: "http://dex:5556/dex", next: cap}
	req, err := http.NewRequest(http.MethodGet, "http://example.com/other", nil)
	require.NoError(t, err)
	_, err = tr.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, "http://example.com/other", cap.gotURL, "non-matching URL must not be rewritten")
}

func TestGenerateState(t *testing.T) {
	s1, err := generateState()
	require.NoError(t, err)
	s2, err := generateState()
	require.NoError(t, err)
	assert.NotEqual(t, s1, s2, "each state must be unique")
	assert.Len(t, s1, 32, "state must be a 32-character hex string (16 bytes)")
}

func TestHandleMe_NoCookie(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := NewServer(Config{Token: "t", LegacyAgentToken: "t"}, pg)
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/api/v1/auth/me")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHandleMe_InvalidCookie(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := NewServer(Config{Token: "t", LegacyAgentToken: "t"}, pg)
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	req, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/api/v1/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "invalid-token"})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHandleMe_ValidSession(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := NewServer(Config{Token: "t", LegacyAgentToken: "t"}, pg)
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	// Create a session directly in the DB.
	sessionToken := "exc_testtoken1234567890abcdef12345678"
	tokenHash := HashToken(sessionToken)
	expiresAt := time.Now().Add(1 * time.Hour)
	_, err := pg.CreateSession(context.Background(), tokenHash, "sub123", "test@example.com", "admin", []byte("dek"), []byte("ct"), expiresAt)
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/api/v1/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleLogout_ClearsSession(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := NewServer(Config{Token: "t", LegacyAgentToken: "t"}, pg)
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	sessionToken := "exc_logouttoken1234567890abcdef1234"
	tokenHash := HashToken(sessionToken)
	_, err := pg.CreateSession(context.Background(), tokenHash, "sub456", "logout@example.com", "admin", []byte("dek"), []byte("ct"), time.Now().Add(time.Hour))
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	// Verify that /me returns 401 after logout.
	req2, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/api/v1/auth/me", nil)
	req2.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp2.StatusCode)
}

func TestHandleOIDCLogin_NotConfigured(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := NewServer(Config{Token: "t", LegacyAgentToken: "t"}, pg)
	// OIDCConfig is not configured.
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(httpSrv.URL + "/api/v1/auth/oidc-login")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleOIDCLogin_NoClientSecret(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := NewServer(Config{Token: "t", LegacyAgentToken: "t"}, pg)
	// No ClientSecret (CLI device flow only configuration).
	srv.SetOIDCConfig(&OIDCConfig{Issuer: "https://example.com", ClientID: "cid"})
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(httpSrv.URL + "/api/v1/auth/oidc-login")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
