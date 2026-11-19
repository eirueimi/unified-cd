package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const relaySecret = "relay-dev-secret-9f3a"

// relayIdP is a fake IdP with discovery, device, token and revocation
// endpoints. It records the form of every request it receives.
type relayIdP struct {
	*testIssuer
	URL string

	mu         sync.Mutex
	deviceForm url.Values
	tokenForms []url.Values
	revokeForm url.Values
	tokenFn    func(form url.Values) (int, any)

	discoveryCalls atomic.Int32
	discoveryGate  chan struct{} // when non-nil, discovery waits until it is closed
	failDiscovery  atomic.Bool
}

func newRelayIdP(t *testing.T, withRevoke bool) *relayIdP {
	t.Helper()
	p := &relayIdP{testIssuer: newTestIssuer(t)}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		p.discoveryCalls.Add(1)
		if g := p.discoveryGate; g != nil {
			select {
			case <-g:
			case <-r.Context().Done():
				return
			}
		}
		if p.failDiscovery.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		doc := map[string]any{
			"issuer":                                p.URL,
			"authorization_endpoint":                p.URL + "/auth",
			"token_endpoint":                        p.URL + "/token",
			"device_authorization_endpoint":         p.URL + "/device/code",
			"jwks_uri":                              p.testIssuer.URL,
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		if withRevoke {
			doc["revocation_endpoint"] = p.URL + "/revoke"
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/device/code", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		p.deviceForm = r.PostForm
		p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc-1", "user_code": "ABCD-EFGH", "verification_uri": p.URL + "/device",
			"verification_uri_complete": p.URL + "/device?c=ABCD-EFGH", "expires_in": 300, "interval": 5,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		p.tokenForms = append(p.tokenForms, r.PostForm)
		fn := p.tokenFn
		p.mu.Unlock()
		code, body := http.StatusOK, any(map[string]any{})
		if fn != nil {
			code, body = fn(r.PostForm)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		p.revokeForm = r.PostForm
		p.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	p.Name = srv.URL
	return p
}

func (p *relayIdP) lastToken() url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tokenForms[len(p.tokenForms)-1]
}

func newRelayServer(t *testing.T, p *relayIdP, secret string) *Server {
	t.Helper()
	s := NewServer(Config{Token: "t"}, nil)
	s.SetOIDCConfig(&OIDCConfig{Issuer: p.URL, ClientID: "web", ClientSecret: "web-secret", DeviceClientID: "cli", DeviceClientSecret: secret})
	return s
}

func relayPost(s *Server, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.RemoteAddr = "192.0.2.10:4444"
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func TestCLIRelay_DeviceStartUsesDeviceClient(t *testing.T) {
	p := newRelayIdP(t, false)
	s := newRelayServer(t, p, relaySecret)
	rec := relayPost(s, "/api/v1/auth/cli/device/start", struct{}{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got api.CLIDeviceStartResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "dc-1", got.DeviceCode)
	assert.Equal(t, "ABCD-EFGH", got.UserCode)
	assert.Equal(t, 300, got.ExpiresIn)
	assert.Equal(t, 5, got.Interval)
	assert.Equal(t, "cli", p.deviceForm.Get("client_id"))
	assert.Contains(t, p.deviceForm.Get("scope"), "openid")
	assert.False(t, strings.Contains(rec.Body.String(), relaySecret))
}

func TestCLIRelay_DeviceStart404WithoutSecret(t *testing.T) {
	p := newRelayIdP(t, false)
	s := newRelayServer(t, p, "")
	rec := relayPost(s, "/api/v1/auth/cli/device/start", struct{}{})
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = relayPost(s, "/api/v1/auth/cli/device/token", api.CLIDeviceTokenRequest{DeviceCode: "x"})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestCLIRelay_DeviceTokenSendsSecretAndExpiryFromIDToken(t *testing.T) {
	p := newRelayIdP(t, false)
	exp := time.Now().Add(37 * time.Minute).Truncate(time.Second)
	p.tokenFn = func(url.Values) (int, any) {
		return 200, map[string]any{
			"id_token":      p.Sign(map[string]any{"aud": "cli", "sub": "u1", "exp": exp.Unix()}),
			"refresh_token": "rt-1", "expires_in": 5,
		}
	}
	s := newRelayServer(t, p, relaySecret)
	rec := relayPost(s, "/api/v1/auth/cli/device/token", api.CLIDeviceTokenRequest{DeviceCode: "dc-1"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got api.CLITokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "rt-1", got.RefreshToken)
	assert.True(t, got.Expiry.Equal(exp), "expiry %v want %v", got.Expiry, exp)
	form := p.lastToken()
	assert.Equal(t, relaySecret, form.Get("client_secret"))
	assert.Equal(t, "cli", form.Get("client_id"))
	assert.Equal(t, "dc-1", form.Get("device_code"))
	assert.Equal(t, "urn:ietf:params:oauth:grant-type:device_code", form.Get("grant_type"))
	assert.Len(t, p.tokenForms, 1, "exactly one upstream token call")
}

func TestCLIRelay_DeviceTokenPendingPassthrough(t *testing.T) {
	p := newRelayIdP(t, false)
	p.tokenFn = func(url.Values) (int, any) {
		return 400, map[string]any{"error": "authorization_pending"}
	}
	s := newRelayServer(t, p, relaySecret)
	rec := relayPost(s, "/api/v1/auth/cli/device/token", api.CLIDeviceTokenRequest{DeviceCode: "dc-1"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"authorization_pending"}`, rec.Body.String())
	assert.Len(t, p.tokenForms, 1, "no server-side polling")
}

func TestCLIRelay_UpstreamErrorBodyNotEchoed(t *testing.T) {
	p := newRelayIdP(t, false)
	p.tokenFn = func(form url.Values) (int, any) {
		return 400, map[string]any{
			"error": "access_denied", "error_description": "user said no",
			"debug": "client_secret=" + form.Get("client_secret"), "request_url": p.URL + "/token?client_secret=" + form.Get("client_secret"),
		}
	}
	s := newRelayServer(t, p, relaySecret)
	rec := relayPost(s, "/api/v1/auth/cli/device/token", api.CLIDeviceTokenRequest{DeviceCode: "dc-1"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"access_denied","errorDescription":"user said no"}`, rec.Body.String())
	assert.False(t, strings.Contains(rec.Body.String(), relaySecret))

	// A non-OAuth upstream failure never leaks the body or URL either.
	p.tokenFn = func(form url.Values) (int, any) {
		return 500, map[string]any{"oops": form.Get("client_secret")}
	}
	rec = relayPost(s, "/api/v1/auth/cli/device/token", api.CLIDeviceTokenRequest{DeviceCode: "dc-1"})
	assert.GreaterOrEqual(t, rec.Code, 400)
	assert.False(t, strings.Contains(rec.Body.String(), relaySecret))
	assert.False(t, strings.Contains(rec.Body.String(), p.URL))

	// A secret echoed inside error_description is redacted.
	p.tokenFn = func(form url.Values) (int, any) {
		return 400, map[string]any{"error": "invalid_request", "error_description": "bad secret " + form.Get("client_secret")}
	}
	rec = relayPost(s, "/api/v1/auth/cli/device/token", api.CLIDeviceTokenRequest{DeviceCode: "dc-1"})
	assert.False(t, strings.Contains(rec.Body.String(), relaySecret))
}

func TestCLIRelay_RefreshBrowserUsesWebClient(t *testing.T) {
	p := newRelayIdP(t, false)
	p.tokenFn = func(form url.Values) (int, any) {
		return 200, map[string]any{"id_token": p.Sign(map[string]any{"aud": form.Get("client_id"), "sub": "u1"}), "refresh_token": "rt-2"}
	}
	s := newRelayServer(t, p, relaySecret)
	rec := relayPost(s, "/api/v1/auth/cli/refresh", api.CLIRefreshRequest{RefreshToken: "rt-1", Client: "browser"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	form := p.lastToken()
	assert.Equal(t, "web", form.Get("client_id"))
	assert.Equal(t, "web-secret", form.Get("client_secret"))
	assert.Equal(t, "refresh_token", form.Get("grant_type"))
	assert.Equal(t, "rt-1", form.Get("refresh_token"))

	rec = relayPost(s, "/api/v1/auth/cli/refresh", api.CLIRefreshRequest{RefreshToken: "rt-1", Client: "device"})
	require.Equal(t, http.StatusOK, rec.Code)
	form = p.lastToken()
	assert.Equal(t, "cli", form.Get("client_id"))
	assert.Equal(t, relaySecret, form.Get("client_secret"))
}

func TestCLIRelay_RefreshWithoutIDToken(t *testing.T) {
	p := newRelayIdP(t, false)
	p.tokenFn = func(url.Values) (int, any) { return 200, map[string]any{"access_token": "at"} }
	s := newRelayServer(t, p, relaySecret)
	rec := relayPost(s, "/api/v1/auth/cli/refresh", api.CLIRefreshRequest{RefreshToken: "rt-1", Client: "device"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"invalid_grant","errorDescription":"identity provider returned no id_token on refresh"}`, rec.Body.String())
}

func TestCLIRelay_RefreshNonRotatingEchoesToken(t *testing.T) {
	p := newRelayIdP(t, false)
	p.tokenFn = func(url.Values) (int, any) {
		return 200, map[string]any{"id_token": p.Sign(map[string]any{"aud": "cli", "sub": "u1"})}
	}
	s := newRelayServer(t, p, relaySecret)
	rec := relayPost(s, "/api/v1/auth/cli/refresh", api.CLIRefreshRequest{RefreshToken: "rt-keep", Client: "device"})
	require.Equal(t, http.StatusOK, rec.Code)
	var got api.CLITokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "rt-keep", got.RefreshToken)
}

func TestCLIRelay_RejectsWrongAudience(t *testing.T) {
	p := newRelayIdP(t, false)
	p.tokenFn = func(url.Values) (int, any) {
		return 200, map[string]any{"id_token": p.Sign(map[string]any{"aud": "someone-else", "sub": "u1"}), "refresh_token": "rt-new"}
	}
	s := newRelayServer(t, p, relaySecret)
	rec := relayPost(s, "/api/v1/auth/cli/refresh", api.CLIRefreshRequest{RefreshToken: "rt-1", Client: "device"})
	assert.GreaterOrEqual(t, rec.Code, 400)
	assert.NotContains(t, rec.Body.String(), "rt-new")
	assert.NotContains(t, rec.Body.String(), "idToken")
}

func TestCLIRelay_Revoke(t *testing.T) {
	p := newRelayIdP(t, false)
	s := newRelayServer(t, p, relaySecret)
	rec := relayPost(s, "/api/v1/auth/cli/revoke", api.CLIRefreshRequest{RefreshToken: "rt-1", Client: "device"})
	assert.Equal(t, http.StatusNotFound, rec.Code, "no revocation endpoint")

	p2 := newRelayIdP(t, true)
	s2 := newRelayServer(t, p2, relaySecret)
	rec = relayPost(s2, "/api/v1/auth/cli/revoke", api.CLIRefreshRequest{RefreshToken: "rt-1", Client: "device"})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, "rt-1", p2.revokeForm.Get("token"))
	assert.Equal(t, "refresh_token", p2.revokeForm.Get("token_type_hint"))
	assert.Equal(t, "cli", p2.revokeForm.Get("client_id"))
	assert.Equal(t, relaySecret, p2.revokeForm.Get("client_secret"))
}

func TestCLIRelay_RateLimit(t *testing.T) {
	p := newRelayIdP(t, false)
	s := newRelayServer(t, p, relaySecret)
	for i := 0; i < 20; i++ {
		rec := relayPost(s, "/api/v1/auth/cli/device/start", struct{}{})
		require.Equal(t, http.StatusOK, rec.Code, "request %d", i+1)
	}
	rec := relayPost(s, "/api/v1/auth/cli/device/start", struct{}{})
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestCLIRelay_LogsCarryNoSecretsOrTokens(t *testing.T) {
	buf := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	p := newRelayIdP(t, true)
	var idTok string
	p.tokenFn = func(url.Values) (int, any) {
		idTok = p.Sign(map[string]any{"aud": "cli", "sub": "u1"})
		return 200, map[string]any{"id_token": idTok, "refresh_token": "rt-secret-value"}
	}
	s := newRelayServer(t, p, relaySecret)
	rec := relayPost(s, "/api/v1/auth/cli/device/token", api.CLIDeviceTokenRequest{DeviceCode: "dc-secret-code"})
	require.Equal(t, http.StatusOK, rec.Code)
	p.tokenFn = func(form url.Values) (int, any) {
		return 400, map[string]any{"error": "invalid_grant", "debug": form.Get("client_secret") + form.Get("refresh_token")}
	}
	relayPost(s, "/api/v1/auth/cli/refresh", api.CLIRefreshRequest{RefreshToken: "rt-secret-value", Client: "device"})
	relayPost(s, "/api/v1/auth/cli/revoke", api.CLIRefreshRequest{RefreshToken: "rt-secret-value", Client: "device"})

	logs := buf.String()
	for _, needle := range []string{relaySecret, "rt-secret-value", idTok, "dc-secret-code"} {
		assert.False(t, strings.Contains(logs, needle), "log leaked a secret")
	}
}

func TestCLIRelay_ErrorDescriptionEchoingTokensIsDropped(t *testing.T) {
	p := newRelayIdP(t, true)
	s := newRelayServer(t, p, relaySecret)

	// (1) refresh: description echoes the presented refresh token.
	p.tokenFn = func(form url.Values) (int, any) {
		return 400, map[string]any{"error": "invalid_grant", "error_description": "refresh token " + form.Get("refresh_token") + " is expired"}
	}
	rec := relayPost(s, "/api/v1/auth/cli/refresh", api.CLIRefreshRequest{RefreshToken: "rt-echo-123", Client: "device"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"invalid_grant"}`, rec.Body.String())

	// (2) device/token: description echoes the device code.
	p.tokenFn = func(form url.Values) (int, any) {
		return 400, map[string]any{"error": "expired_token", "error_description": "code " + form.Get("device_code") + " expired"}
	}
	rec = relayPost(s, "/api/v1/auth/cli/device/token", api.CLIDeviceTokenRequest{DeviceCode: "dc-echo-456"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"expired_token"}`, rec.Body.String())

	// Revoke failures are held to the same rule.
	p.tokenFn = nil

	// (3) a secret with +/= echoed form-encoded.
	const odd = "ab+cd/ef=="
	s3 := newRelayServer(t, p, odd)
	p.tokenFn = func(form url.Values) (int, any) {
		return 400, map[string]any{"error": "invalid_client", "error_description": "bad client_secret=" + url.QueryEscape(form.Get("client_secret"))}
	}
	rec = relayPost(s3, "/api/v1/auth/cli/refresh", api.CLIRefreshRequest{RefreshToken: "rt-1", Client: "device"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"invalid_client"}`, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), odd)
	assert.NotContains(t, rec.Body.String(), url.QueryEscape(odd))

	// A harmless description is kept and truncated on a rune boundary.
	p.tokenFn = func(url.Values) (int, any) {
		return 400, map[string]any{"error": "invalid_request", "error_description": strings.Repeat("é", 400)}
	}
	rec = relayPost(s, "/api/v1/auth/cli/refresh", api.CLIRefreshRequest{RefreshToken: "rt-1", Client: "device"})
	var got api.CLIOAuthError
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.LessOrEqual(t, len(got.ErrorDescription), 512)
	assert.True(t, utf8.ValidString(got.ErrorDescription))
	assert.NotEmpty(t, got.ErrorDescription)
}

func TestCLIRelay_ErrorCodeRestrictedToLowerUnderscore(t *testing.T) {
	p := newRelayIdP(t, false)
	s := newRelayServer(t, p, relaySecret)
	for _, bad := range []string{"Invalid-Grant", "oops rt-1", "a/b", "ünï"} {
		p.tokenFn = func(url.Values) (int, any) {
			return 400, map[string]any{"error": bad, "error_description": "x"}
		}
		rec := relayPost(s, "/api/v1/auth/cli/refresh", api.CLIRefreshRequest{RefreshToken: "rt-1", Client: "device"})
		assert.NotContains(t, rec.Body.String(), bad)
		assert.Contains(t, rec.Body.String(), "server_error")
	}
}

func TestCLIRelay_DiscoveryIsCachedForTenMinutes(t *testing.T) {
	p := newRelayIdP(t, false)
	s := newRelayServer(t, p, relaySecret)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.relayDisc.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		rec := relayPost(s, "/api/v1/auth/cli/device/start", struct{}{})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	assert.Equal(t, int32(1), p.discoveryCalls.Load(), "second call within the TTL reuses the discovery")

	now = now.Add(9*time.Minute + 59*time.Second)
	require.Equal(t, http.StatusOK, relayPost(s, "/api/v1/auth/cli/device/start", struct{}{}).Code)
	assert.Equal(t, int32(1), p.discoveryCalls.Load())

	now = now.Add(2 * time.Second)
	require.Equal(t, http.StatusOK, relayPost(s, "/api/v1/auth/cli/device/start", struct{}{}).Code)
	assert.Equal(t, int32(2), p.discoveryCalls.Load(), "discovery runs again after the TTL")
}

func TestCLIRelay_FailedDiscoveryIsNotCached(t *testing.T) {
	p := newRelayIdP(t, false)
	s := newRelayServer(t, p, relaySecret)
	p.failDiscovery.Store(true)
	rec := relayPost(s, "/api/v1/auth/cli/device/start", struct{}{})
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Equal(t, int32(1), p.discoveryCalls.Load())

	p.failDiscovery.Store(false)
	rec = relayPost(s, "/api/v1/auth/cli/device/start", struct{}{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, int32(2), p.discoveryCalls.Load(), "the failure was retried")
}

func TestCLIRelay_DiscoveryCacheKeepsPerRequestRedirectURL(t *testing.T) {
	p := newRelayIdP(t, false)
	s := newRelayServer(t, p, relaySecret)
	_, _, cfgA, err := s.relayProvider(t.Context(), "a.example.com")
	require.NoError(t, err)
	_, _, cfgB, err := s.relayProvider(t.Context(), "b.example.com")
	require.NoError(t, err)
	assert.Equal(t, "http://a.example.com/api/v1/auth/oidc-callback", cfgA.RedirectURL)
	assert.Equal(t, "http://b.example.com/api/v1/auth/oidc-callback", cfgB.RedirectURL)
	assert.Equal(t, int32(1), p.discoveryCalls.Load())
}

func TestCLIRelay_DiscoveryCacheConcurrent(t *testing.T) {
	p := newRelayIdP(t, false)
	s := newRelayServer(t, p, relaySecret)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, err := s.relayProvider(t.Context(), "h")
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), p.discoveryCalls.Load())
}

// blockingDiscoveryIdP serves discovery only after release is closed.
func blockingDiscoveryIdP(t *testing.T) (*relayIdP, chan struct{}) {
	t.Helper()
	p := newRelayIdP(t, false)
	release := make(chan struct{})
	p.discoveryGate = release
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	return p, release
}

func TestCLIRelay_ConcurrentCallersShareOneDiscovery(t *testing.T) {
	p, release := blockingDiscoveryIdP(t)
	s := newRelayServer(t, p, relaySecret)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, err := s.relayProvider(t.Context(), "h")
			assert.NoError(t, err)
		}()
	}
	require.Eventually(t, func() bool { return p.discoveryCalls.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	assert.Equal(t, int32(1), p.discoveryCalls.Load(), "exactly one discovery request for all callers")
}

func TestCLIRelay_CancelledCallerReturnsWhileDiscoveryContinues(t *testing.T) {
	p, release := blockingDiscoveryIdP(t)
	s := newRelayServer(t, p, relaySecret)

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() {
		_, _, _, err := s.relayProvider(ctx, "h")
		errc <- err
	}()
	require.Eventually(t, func() bool { return p.discoveryCalls.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled caller must return promptly")
	}

	// The shared discovery was not cancelled: a second caller gets its result.
	other := make(chan error, 1)
	go func() {
		_, _, _, err := s.relayProvider(t.Context(), "h")
		other <- err
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	require.NoError(t, <-other)
	assert.Equal(t, int32(1), p.discoveryCalls.Load())
}

func TestCLIRelay_HungDiscoveryIsAbandonedAndNotCached(t *testing.T) {
	p, release := blockingDiscoveryIdP(t)
	s := newRelayServer(t, p, relaySecret)
	s.relayDisc.timeout = 150 * time.Millisecond

	start := time.Now()
	_, _, _, err := s.relayProvider(t.Context(), "h")
	require.Error(t, err)
	assert.Less(t, time.Since(start), 3*time.Second, "bounded by the discovery timeout")

	close(release)
	_, _, _, err = s.relayProvider(t.Context(), "h")
	require.NoError(t, err, "the failure was not cached")
	assert.Equal(t, int32(2), p.discoveryCalls.Load())
}

func TestCLIRelay_PanickingDiscoveryBecomesErrorAndClearsFlight(t *testing.T) {
	var c relayDiscoveryCache
	_, err := c.get(t.Context(), "k", func(context.Context) (*oidc.Provider, error) {
		panic("boom")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "panic")

	c.mu.RLock()
	flight := c.flight
	c.mu.RUnlock()
	assert.Nil(t, flight, "the flight is cleared")

	// A later call starts a fresh discovery instead of waiting on the dead one.
	_, err = c.get(t.Context(), "k", func(context.Context) (*oidc.Provider, error) {
		return nil, assert.AnError
	})
	assert.ErrorIs(t, err, assert.AnError)
}
