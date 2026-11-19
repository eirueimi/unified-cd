package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
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
	startStatus int // 0 = serve the confirmation page
	startBody   string
	exchangeErr string
	loopbackErr string // when set, continue hands this error code to the loopback
	redirect    string
	continued   bool
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
				_, _ = w.Write([]byte(f.startBody))
				return
			}
			f.challenge = r.URL.Query().Get("challenge")
			f.redirect = r.URL.Query().Get("redirect")
			http.SetCookie(w, &http.Cookie{Name: "ucd_cli_login", Value: "nonce", Path: "/api/v1/auth/cli/browser/"})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<form method="post" action="/api/v1/auth/cli/browser/continue"><input type="hidden" name="state" value="s1"></form>`))
		case "/api/v1/auth/cli/browser/continue":
			if c, err := r.Cookie("ucd_cli_login"); err != nil || c.Value != "nonce" || r.Method != http.MethodPost {
				http.Error(w, "confirmation required", http.StatusForbidden)
				return
			}
			f.continued = true
			if f.loopbackErr != "" {
				http.Redirect(w, r, f.redirect+"?error="+f.loopbackErr, http.StatusFound)
				return
			}
			http.Redirect(w, r, f.redirect+"?code=one-time-code", http.StatusFound)
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

// stubBrowser replaces the browser opener with a cookie-keeping client that
// loads the start page, clicks Continue (posts the form) and follows redirects
// as a real browser would, and records the final page body.
func stubBrowser(t *testing.T) *string {
	var page string
	old := openBrowser
	openBrowser = func(u string) error {
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar}
		resp, err := client.Get(u) //nolint:gosec
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		page = string(b)
		if strings.Contains(page, `action="/api/v1/auth/cli/browser/continue"`) {
			base, _ := url.Parse(u)
			resp, err = client.PostForm(base.Scheme+"://"+base.Host+"/api/v1/auth/cli/browser/continue", url.Values{"state": {"s1"}})
			if err != nil {
				return err
			}
			b, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			page = string(b)
		}
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
	assert.Contains(t, stdout, "The browser will ask you to confirm the sign-in")
	assert.True(t, f.continued)
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

func TestLoginBrowser_OldController401(t *testing.T) {
	f := newFakeBrowserController(t, true)
	f.startStatus = http.StatusUnauthorized
	f.startBody = "unauthorized\n"
	_, _, err := runLoginCmd(t, "--browser", "--server", f.srv.URL, "--config", filepath.Join(t.TempDir(), "c.yaml"))
	require.Error(t, err)
	assert.Equal(t, "this server does not support browser login for the CLI; upgrade the controller or use the device flow", err.Error())
}

func TestLoginBrowser_StartPlainTextErrors(t *testing.T) {
	f := newFakeBrowserController(t, true)
	f.startStatus = http.StatusServiceUnavailable
	f.startBody = "browser login requires the controller key manager\n"
	_, _, err := runLoginCmd(t, "--browser", "--server", f.srv.URL, "--config", filepath.Join(t.TempDir(), "c.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "browser login requires the controller key manager")

	f.startStatus = http.StatusBadRequest
	f.startBody = "invalid redirect\x1b[31m\nsecond line"
	_, _, err = runLoginCmd(t, "--browser", "--server", f.srv.URL, "--config", filepath.Join(t.TempDir(), "c.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server returned 400: invalid redirect[31m")
	assert.NotContains(t, err.Error(), "second line")
	assert.NotContains(t, err.Error(), "")
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
	h, results := callbackHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/callback", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/other?code=x", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	select {
	case <-results:
		t.Fatal("no result expected")
	default:
	}
}

func TestLoginBrowser_CallbackDeliversErrorCode(t *testing.T) {
	h, results := callbackHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/callback?error=access_denied", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, browserRefusedPage, strings.TrimSpace(rec.Body.String()))
	assert.Equal(t, callbackResult{errCode: "access_denied"}, <-results)

	h, results = callbackHandler()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/callback?error=%1B%5B31mevil", nil))
	assert.Equal(t, callbackResult{errCode: "server_error"}, <-results, "a non-token error code is replaced")
}

func TestLoginBrowser_RefusedLoginStopsAndSavesNothing(t *testing.T) {
	f := newFakeBrowserController(t, true)
	f.loopbackErr = "access_denied"
	page := stubBrowser(t)
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	_, _, err := runLoginCmd(t, "--browser", "--server", f.srv.URL, "--config", path)
	require.Error(t, err)
	assert.Equal(t, "browser login was refused: access_denied", err.Error())
	assert.Contains(t, *page, "Login was refused")
	cfg, _ := LoadConfig(path)
	assert.Empty(t, cfg.Token)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Zero(t, f.exchanged.Code, "no exchange after a refusal")
}

func TestLoginBrowser_PreflightAcceptsConfirmationPageAndRedirect(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusFound} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if status == http.StatusFound {
				w.Header().Set("Location", "https://idp.invalid/auth")
			}
			w.WriteHeader(status)
		}))
		assert.NoError(t, preflightBrowserStart(t.Context(), srv.URL+"/start"), "status %d", status)
		srv.Close()
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
