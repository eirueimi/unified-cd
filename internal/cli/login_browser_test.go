package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
)

// fakeBrowserController is a controller double for the loopback login flow.
type fakeBrowserController struct {
	srv *httptest.Server

	mu          sync.Mutex
	startStatus int // 0 = redirect to the loopback URL
	exchangeErr string
	tok         api.CLITokenResponse
	challenge   string
	exchanged   api.CLIBrowserExchangeRequest
	enabled     bool
}

func newFakeBrowserController(t *testing.T, enabled bool) *fakeBrowserController {
	f := &fakeBrowserController{enabled: enabled}
	f.tok = api.CLITokenResponse{
		IDToken:      testJWT(time.Now().Add(time.Hour)),
		RefreshToken: "refresh-secret",
		Expiry:       time.Now().Add(time.Hour).Truncate(time.Second),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/auth/oidc-config":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": "https://idp.invalid", "clientId": "web", "browserSSOEnabled": f.enabled})
		case "/api/v1/auth/cli/browser/start":
			if f.startStatus != 0 {
				w.WriteHeader(f.startStatus)
				return
			}
			f.challenge = r.URL.Query().Get("challenge")
			redirect := r.URL.Query().Get("redirect")
			http.Redirect(w, r, redirect+"?code=one-time-code", http.StatusFound)
		case "/api/v1/auth/cli/browser/exchange":
			_ = json.NewDecoder(r.Body).Decode(&f.exchanged)
			if f.exchangeErr != "" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(api.CLIOAuthError{Error: f.exchangeErr})
				return
			}
			_ = json.NewEncoder(w).Encode(f.tok)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// stubBrowser replaces the browser opener with a plain GET that follows
// redirects (as a real browser would) and records the final page body.
func stubBrowser(t *testing.T) *string {
	var page string
	old := openBrowser
	openBrowser = func(u string) error {
		resp, err := http.Get(u) //nolint:gosec
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		page = string(b)
		return nil
	}
	t.Cleanup(func() { openBrowser = old })
	return &page
}

func runLoginCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	// Run under the root command so the persistent --config flag exists.
	cmd := NewRoot()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(append([]string{"login"}, args...))
	err = cmd.Execute()
	return out.String(), errb.String(), err
}

func TestLoginBrowser_EndToEnd(t *testing.T) {
	f := newFakeBrowserController(t, true)
	page := stubBrowser(t)
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	require.NoError(t, SaveConfig(path, Config{Server: "http://old.invalid", Token: "old", Headers: []string{"X-Test: 1"}}))

	stdout, stderr, err := runLoginCmd(t, "--browser", "--server", f.srv.URL, "--config", path)
	require.NoError(t, err, stderr)

	assert.Equal(t, "Login complete. You can close this tab.", strings.TrimSpace(*page))
	assert.Contains(t, stdout, f.srv.URL+"/api/v1/auth/cli/browser/start?")
	assert.NotContains(t, stdout+stderr, "one-time-code")
	assert.NotContains(t, stdout+stderr, "refresh-secret")
	assert.Empty(t, stderr)

	// The PKCE pair matches: challenge = S256(verifier), a 32-byte verifier.
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, "one-time-code", f.exchanged.Code)
	sum := sha256.Sum256([]byte(f.exchanged.CodeVerifier))
	assert.Equal(t, base64.RawURLEncoding.EncodeToString(sum[:]), f.challenge)
	raw, err := base64.RawURLEncoding.DecodeString(f.exchanged.CodeVerifier)
	require.NoError(t, err)
	assert.Len(t, raw, 32)

	saved, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, f.srv.URL, saved.Server)
	assert.Equal(t, f.tok.IDToken, saved.Token)
	assert.Equal(t, "refresh-secret", saved.RefreshToken)
	assert.Equal(t, "browser", saved.LoginClient)
	assert.False(t, saved.LoginRelay)
	assert.Equal(t, []string{"X-Test: 1"}, saved.Headers, "existing headers must survive login")
}

func TestLoginBrowser_NoRefreshTokenWarns(t *testing.T) {
	f := newFakeBrowserController(t, true)
	f.tok.RefreshToken = ""
	stubBrowser(t)
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	_, stderr, err := runLoginCmd(t, "--browser", "--server", f.srv.URL, "--config", path)
	require.NoError(t, err)
	assert.Contains(t, stderr, noRefreshTokenWarning)
}

func TestLoginBrowser_RejectsIssuerAndClientID(t *testing.T) {
	for _, extra := range [][]string{{"--issuer", "x"}, {"--client-id", "x"}} {
		args := append([]string{"--browser", "--server", "http://127.0.0.1:1"}, extra...)
		_, _, err := runLoginCmd(t, args...)
		require.Error(t, err)
		assert.Equal(t, "--browser cannot be combined with --issuer or --client-id", err.Error())
	}
}

func TestLoginBrowser_OldController404(t *testing.T) {
	f := newFakeBrowserController(t, true)
	f.startStatus = http.StatusNotFound
	opened := false
	old := openBrowser
	openBrowser = func(string) error { opened = true; return nil }
	t.Cleanup(func() { openBrowser = old })
	_, _, err := runLoginCmd(t, "--browser", "--server", f.srv.URL, "--config", filepath.Join(t.TempDir(), "c.yaml"))
	require.Error(t, err)
	assert.Equal(t, "this server does not support browser login for the CLI; upgrade the controller or use the device flow", err.Error())
	assert.False(t, opened)
}

func TestLoginBrowser_NotEnabled(t *testing.T) {
	f := newFakeBrowserController(t, false)
	_, _, err := runLoginCmd(t, "--browser", "--server", f.srv.URL, "--config", filepath.Join(t.TempDir(), "c.yaml"))
	require.Error(t, err)
	assert.Equal(t, "browser login is not configured on this server", err.Error())
}

func TestLoginBrowser_Timeout(t *testing.T) {
	f := newFakeBrowserController(t, true)
	old, oldOpen := browserLoginTimeout, openBrowser
	browserLoginTimeout = 100 * time.Millisecond
	openBrowser = func(string) error { return nil } // the user never finishes
	t.Cleanup(func() { browserLoginTimeout, openBrowser = old, oldOpen })
	path := filepath.Join(t.TempDir(), "c.yaml")
	_, _, err := runLoginCmd(t, "--browser", "--server", f.srv.URL, "--config", path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
	cfg, _ := LoadConfig(path)
	assert.Empty(t, cfg.Token)
}

func TestLoginBrowser_ExchangeRejected(t *testing.T) {
	f := newFakeBrowserController(t, true)
	f.exchangeErr = "invalid_grant"
	stubBrowser(t)
	_, _, err := runLoginCmd(t, "--browser", "--server", f.srv.URL, "--config", filepath.Join(t.TempDir(), "c.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid_grant")
	assert.NotContains(t, err.Error(), "one-time-code")
}

func TestLoginBrowser_CallbackIgnoresMissingCode(t *testing.T) {
	h, codes := callbackHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/callback", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/other?code=x", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	select {
	case <-codes:
		t.Fatal("no code expected")
	default:
	}
}

// The login commands write to the config path they were invoked with and keep
// the file's headers (device relay flow).
func TestLogin_DeviceRelay_UsesConfigFlagAndKeepsHeaders(t *testing.T) {
	jwt := testJWT(time.Now().Add(time.Hour))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/oidc-config":
			_, _ = w.Write([]byte(`{"issuer":"https://idp.invalid","clientId":"web","deviceRelay":true}`))
		case "/api/v1/auth/cli/device/start":
			_, _ = w.Write([]byte(`{"deviceCode":"d","userCode":"U","verificationUri":"https://v.invalid","expiresIn":60,"interval":1}`))
		case "/api/v1/auth/cli/device/token":
			_ = json.NewEncoder(w).Encode(api.CLITokenResponse{IDToken: jwt, RefreshToken: "r", Expiry: time.Now().Add(time.Hour)})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "sub", "cfg.yaml")
	require.NoError(t, SaveConfig(path, Config{Headers: []string{"Proxy-Authorization: x"}}))

	_, stderr, err := runLoginCmd(t, "--server", srv.URL, "--config", path)
	require.NoError(t, err, stderr)
	saved, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, jwt, saved.Token)
	assert.True(t, saved.LoginRelay)
	assert.Equal(t, []string{"Proxy-Authorization: x"}, saved.Headers)
}

// A PAT login also honours --config and keeps headers.
func TestLogin_TokenPrompt_UsesConfigFlagAndKeepsHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tokens":
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	require.NoError(t, SaveConfig(path, Config{Headers: []string{"X-A: b"}, RefreshToken: "stale", LoginClient: "browser"}))

	cmd := NewRoot()
	cmd.SetIn(strings.NewReader("pat-value\n"))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"login", "--server", srv.URL, "--config", path})
	require.NoError(t, cmd.Execute())

	saved, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "pat-value", saved.Token)
	assert.Equal(t, []string{"X-A: b"}, saved.Headers)
	assert.Empty(t, saved.RefreshToken, "a PAT login must not keep an old SSO refresh token")
	assert.Empty(t, saved.LoginClient)
}
