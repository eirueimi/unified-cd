package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/store/storetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// discoveryIssuer is a fake OIDC issuer serving discovery and JWKS, signing
// tokens with the shared test key.
type discoveryIssuer struct {
	*testIssuer
	URL string
}

func newDiscoveryIssuer(t *testing.T, revocationEndpoint string) *discoveryIssuer {
	t.Helper()
	ti := newTestIssuer(t)
	d := &discoveryIssuer{testIssuer: ti}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{
			"issuer":                                d.URL,
			"authorization_endpoint":                d.URL + "/auth",
			"token_endpoint":                        d.URL + "/token",
			"device_authorization_endpoint":         d.URL + "/device/code",
			"jwks_uri":                              ti.URL,
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		if revocationEndpoint != "" {
			doc["revocation_endpoint"] = revocationEndpoint
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	d.URL = srv.URL
	ti.Name = srv.URL
	return d
}

func TestBearerAudiences(t *testing.T) {
	cases := []struct {
		name string
		cfg  OIDCConfig
		want []string
	}{
		{"device client first", OIDCConfig{ClientID: "web", DeviceClientID: "cli"}, []string{"cli", "web"}},
		{"no device client", OIDCConfig{ClientID: "web"}, []string{"web"}},
		{"duplicates removed", OIDCConfig{ClientID: "web", DeviceClientID: "web"}, []string{"web"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(Config{}, nil)
			cfg := tc.cfg
			s.SetOIDCConfig(&cfg)
			assert.Equal(t, tc.want, s.bearerAudiences())
		})
	}
}

func TestVerifyOIDCBearer_AudienceAllowList(t *testing.T) {
	iss := newDiscoveryIssuer(t, "")
	s := NewServer(Config{}, nil)
	s.SetOIDCConfig(&OIDCConfig{Issuer: iss.URL, ClientID: "web", DeviceClientID: "cli"})

	for _, aud := range []string{"web", "cli"} {
		tok := iss.Sign(map[string]any{"aud": aud, "sub": "u1"})
		got, err := s.verifyOIDCBearer(t.Context(), tok)
		require.NoError(t, err, aud)
		assert.Equal(t, "u1", got.Subject)
		claims, err := s.verifyBearerClaims(t.Context(), tok)
		require.NoError(t, err, aud)
		assert.Equal(t, "u1", claims["sub"])
	}

	other := iss.Sign(map[string]any{"aud": "other", "sub": "u1"})
	_, err := s.verifyOIDCBearer(t.Context(), other)
	require.Error(t, err)
	_, err = s.verifyBearerClaims(t.Context(), other)
	require.Error(t, err)

	expired := iss.Sign(map[string]any{"aud": "web", "sub": "u1", "exp": 1})
	_, err = s.verifyOIDCBearer(t.Context(), expired)
	require.Error(t, err, "expiry is still enforced")
}

func TestOIDCConfig_DeviceRelayAndRevocation(t *testing.T) {
	const secret = "dev-secret-do-not-leak-7731"
	iss := newDiscoveryIssuer(t, "https://idp.example.com/revoke")

	get := func(cfg OIDCConfig) (string, map[string]any) {
		s := NewServer(Config{}, nil)
		s.SetOIDCConfig(&cfg)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc-config", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		var m map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))
		return rec.Body.String(), m
	}

	body, m := get(OIDCConfig{Issuer: iss.URL, ClientID: "web", DeviceClientID: "cli", DeviceClientSecret: secret})
	assert.Equal(t, true, m["deviceRelay"])
	assert.False(t, strings.Contains(body, secret), "response must never carry the device client secret")
	assert.Equal(t, "https://idp.example.com/revoke", m["revocationEndpoint"])

	_, m = get(OIDCConfig{Issuer: iss.URL, ClientID: "web", DeviceClientID: "cli"})
	assert.Equal(t, false, m["deviceRelay"])

	plain := newDiscoveryIssuer(t, "")
	_, m = get(OIDCConfig{Issuer: plain.URL, ClientID: "web"})
	_, has := m["revocationEndpoint"]
	assert.False(t, has, "revocationEndpoint omitted when the issuer has none")
}

func TestAuthCodeOptions(t *testing.T) {
	iss := newDiscoveryIssuer(t, "")
	s := NewServer(Config{}, nil)
	s.SetOIDCConfig(&OIDCConfig{Issuer: iss.URL, ClientID: "web", ClientSecret: "x", AuthParams: map[string]string{"access_type": "offline", "prompt": "consent"}})
	_, _, cfg, err := s.oidcProvider(t.Context(), "localhost")
	require.NoError(t, err)
	u, err := url.Parse(cfg.AuthCodeURL("st", s.authCodeOptions()...))
	require.NoError(t, err)
	assert.Equal(t, "offline", u.Query().Get("access_type"))
	assert.Equal(t, "consent", u.Query().Get("prompt"))

	s.oidcCfg.AuthParams = nil
	assert.Empty(t, s.authCodeOptions())
}

func TestHandleOIDCLogin_AppliesAuthParams(t *testing.T) {
	iss := newDiscoveryIssuer(t, "")
	pg := storetest.NewTestPostgres(t)
	srv := NewServer(Config{}, pg)
	srv.SetOIDCConfig(&OIDCConfig{Issuer: iss.URL, ClientID: "web", ClientSecret: "x", AuthParams: map[string]string{"access_type": "offline"}})
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(httpSrv.URL + "/api/v1/auth/oidc-login")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode)
	loc, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "offline", loc.Query().Get("access_type"))
}
