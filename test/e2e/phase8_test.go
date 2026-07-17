package e2e

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/eirueimi/unified-cd/internal/store"
)

// newTestKeyManager creates a LocalKeyManager with a random master key for testing.
func newTestKeyManager(t *testing.T) secrets.KeyManager {
	t.Helper()
	// Generate a 32-byte random master key
	rawKey := make([]byte, 32)
	_, err := rand.Read(rawKey)
	require.NoError(t, err)
	// NewLocalKeyManager accepts a hex string (64 characters)
	hexKey := make([]byte, 64)
	const hextable = "0123456789abcdef"
	for i, b := range rawKey {
		hexKey[i*2] = hextable[b>>4]
		hexKey[i*2+1] = hextable[b&0x0f]
	}
	km, err := secrets.NewLocalKeyManager(string(hexKey))
	require.NoError(t, err)
	return km
}

// mockIdP returns an httptest.Server that mocks the OIDC endpoints.
func mockIdP(t *testing.T, privateKey *rsa.PrivateKey, sub, email string) *httptest.Server {
	t.Helper()
	var idpSrv *httptest.Server
	idpSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"issuer":                 idpSrv.URL,
				"authorization_endpoint": idpSrv.URL + "/auth",
				"token_endpoint":         idpSrv.URL + "/token",
				"jwks_uri":               idpSrv.URL + "/keys",
			})
		case "/keys":
			jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
				{Key: &privateKey.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"},
			}}
			json.NewEncoder(w).Encode(jwks)
		case "/auth":
			// Issue a code and redirect to the callback URL
			callbackURL := r.URL.Query().Get("redirect_uri")
			state := r.URL.Query().Get("state")
			redirect := callbackURL + "?code=test-code&state=" + url.QueryEscape(state)
			http.Redirect(w, r, redirect, http.StatusFound)
		case "/token":
			// Return id_token and refresh_token
			// The jwt sub-package of go-jose/v4 is not included in vendor,
			// so the JWT is manually constructed via JWS Compact Serialization
			sig, _ := jose.NewSigner(
				jose.SigningKey{Algorithm: jose.RS256, Key: privateKey},
				(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"),
			)
			now := time.Now()
			payload := map[string]interface{}{
				"iss":   idpSrv.URL,
				"sub":   sub,
				"aud":   "test-client",
				"exp":   now.Add(time.Hour).Unix(),
				"iat":   now.Unix(),
				"email": email,
			}
			payloadBytes, _ := json.Marshal(payload)
			jws, _ := sig.Sign(payloadBytes)
			// CompactSerialize() returns a JWT in header.base64url(payload).signature format.
			// The go-oidc Verifier uses go-jose's ParseSigned, which is compatible with this format.
			compact, _ := jws.CompactSerialize()
			idToken := compact
			// The oauth2 library checks the response Content-Type to decide how to parse it, so this must be set
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token":  "access-token",
				"token_type":    "Bearer",
				"id_token":      idToken,
				"refresh_token": "refresh-token-123",
				"expires_in":    3600,
			})
		}
	}))
	return idpSrv
}

func TestPhase8_OIDCConfig_NotConfigured(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", LegacyAgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/api/v1/auth/oidc-config")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPhase8_OIDCConfig_WithoutSecret(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", LegacyAgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	srv.SetOIDCConfig(&controller.OIDCConfig{Issuer: "https://example.com", ClientID: "cid"})
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/api/v1/auth/oidc-config")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	assert.Equal(t, false, body["browserSSOEnabled"])
}

func TestPhase8_OIDCConfig_WithSecret(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", LegacyAgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	srv.SetOIDCConfig(&controller.OIDCConfig{Issuer: "https://example.com", ClientID: "cid", ClientSecret: "secret"})
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/api/v1/auth/oidc-config")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	assert.Equal(t, true, body["browserSSOEnabled"])
}

func TestPhase8_FullOIDCFlow(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	idpSrv := mockIdP(t, privateKey, "user-sub-123", "testuser@example.com")
	defer idpSrv.Close()

	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "t", LegacyAgentToken: "t"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "t"))
	km := newTestKeyManager(t)
	srv.SetKeyManager(km)
	srv.SetOIDCConfig(&controller.OIDCConfig{
		Issuer:       idpSrv.URL,
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		// After the RBAC merge, OIDC login is deny-by-default when no role can
		// be resolved from the ID token claims. The mock IdP token in this test
		// carries no groups/role claim, so a DefaultRole is required for login
		// to succeed.
		DefaultRole: "admin",
	})
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	// Use a cookie jar to automatically retain session cookies
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// 1. /oidc-login → generate state → IdP /auth → /oidc-callback → redirect to /ui/
	resp, err := client.Get(httpSrv.URL + "/api/v1/auth/oidc-login")
	require.NoError(t, err)
	defer resp.Body.Close()
	// Final redirect destination should be /ui/
	assert.True(t, strings.HasSuffix(resp.Request.URL.Path, "/ui/") || resp.StatusCode == http.StatusOK)

	// 2. /auth/me should return 200 with the correct user information
	resp2, err := client.Get(httpSrv.URL + "/api/v1/auth/me")
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	var me map[string]string
	json.NewDecoder(resp2.Body).Decode(&me)
	assert.Equal(t, "user-sub-123", me["sub"])
	assert.Equal(t, "testuser@example.com", me["email"])

	// 3. API endpoints should be accessible using the session cookie
	resp3, err := client.Get(httpSrv.URL + "/api/v1/jobs")
	require.NoError(t, err)
	defer resp3.Body.Close()
	assert.Equal(t, http.StatusOK, resp3.StatusCode)

	// 4. After logout, /me should return 401
	logoutReq, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/auth/logout", nil)
	resp4, err := client.Do(logoutReq)
	require.NoError(t, err)
	defer resp4.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp4.StatusCode)

	resp5, err := client.Get(httpSrv.URL + "/api/v1/auth/me")
	require.NoError(t, err)
	defer resp5.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp5.StatusCode)
}

func TestPhase8_PATAndSessionCoexist(t *testing.T) {
	pg := store.NewTestPostgres(t)
	srv := controller.NewServer(controller.Config{Token: "static-token", LegacyAgentToken: "static-token"}, pg)
	require.NoError(t, mustSeedBootstrapPAT(t, pg, "static-token"))
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	// Should be able to authenticate with PAT
	req, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer static-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
