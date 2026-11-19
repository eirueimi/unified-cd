package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/eirueimi/unified-cd/internal/store/storetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	cliLoopback = "http://127.0.0.1:5555/callback"
	cliVerifier = "verifier-0123456789-0123456789-0123456789-abcdef"
)

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// codeCountingStore counts the one-time codes the controller stores.
type codeCountingStore struct {
	store.Store
	created atomic.Int32
}

func (c *codeCountingStore) CreateCLILoginCode(ctx context.Context, codeHash, challenge string, dek, ct []byte, expiresAt time.Time) (string, error) {
	c.created.Add(1)
	return c.Store.CreateCLILoginCode(ctx, codeHash, challenge, dek, ct, expiresAt)
}

type browserFixture struct {
	idp    *relayIdP
	s      *Server
	st     *codeCountingStore
	groups []string
	exp    time.Time
}

func newBrowserFixture(t *testing.T, withKM bool) *browserFixture {
	t.Helper()
	f := &browserFixture{idp: newRelayIdP(t, false), exp: time.Now().Add(41 * time.Minute).Truncate(time.Second), groups: []string{"admins"}}
	f.idp.tokenFn = func(form url.Values) (int, any) {
		claims := map[string]any{"aud": "web", "sub": "u1", "email": "u1@example.com", "exp": f.exp.Unix()}
		if f.groups != nil {
			claims["groups"] = f.groups
		}
		return 200, map[string]any{"id_token": f.idp.Sign(claims), "refresh_token": "rt-browser", "access_token": "at"}
	}
	f.st = &codeCountingStore{Store: storetest.NewTestPostgres(t)}
	f.s = NewServer(Config{Token: "t"}, f.st)
	t.Cleanup(f.s.Close)
	if withKM {
		f.s.SetKeyManager(testKeyManager(t))
	}
	f.s.SetOIDCConfig(&OIDCConfig{
		Issuer: f.idp.URL, ClientID: "web", ClientSecret: "web-secret",
		RoleMap:    map[string]string{"admins": "admin"},
		AuthParams: map[string]string{"access_type": "offline"},
	})
	return f
}

func (f *browserFixture) do(method, target string, body any) *httptest.ResponseRecorder {
	rd := bytes.NewReader(nil)
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, target, rd)
	req.RemoteAddr = "192.0.2.10:4444"
	rec := httptest.NewRecorder()
	f.s.Router().ServeHTTP(rec, req)
	return rec
}

func (f *browserFixture) exchange(code, verifier string) *httptest.ResponseRecorder {
	return f.do(http.MethodPost, "/api/v1/auth/cli/browser/exchange", api.CLIBrowserExchangeRequest{Code: code, CodeVerifier: verifier})
}

func startURL(redirect, challenge string) string {
	q := url.Values{}
	if redirect != "" {
		q.Set("redirect", redirect)
	}
	if challenge != "" {
		q.Set("challenge", challenge)
	}
	return "/api/v1/auth/cli/browser/start?" + q.Encode()
}

// callbackURL is the IdP callback for a redirect Location that carries state.
func callbackURL(rec *httptest.ResponseRecorder) string {
	loc, _ := url.Parse(rec.Header().Get("Location"))
	return callbackForState(loc.Query().Get("state"))
}

func callbackForState(state string) string {
	return "/api/v1/auth/oidc-callback?state=" + url.QueryEscape(state) + "&code=idp-code"
}

var stateInputRE = regexp.MustCompile(`name="state" value="([^"]+)"`)

// startPage runs the start request and returns the state shown in the page and
// the confirmation cookie it set.
func (f *browserFixture) startPage(t *testing.T, redirect string) (rec *httptest.ResponseRecorder, state string, cookie *http.Cookie) {
	t.Helper()
	rec = f.do(http.MethodGet, startURL(redirect, pkceChallenge(cliVerifier)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	m := stateInputRE.FindStringSubmatch(rec.Body.String())
	require.Len(t, m, 2, "page must carry the state: %s", rec.Body.String())
	for _, c := range rec.Result().Cookies() {
		if c.Name == cliConfirmCookie {
			cookie = c
		}
	}
	require.NotNil(t, cookie, "start must set the confirmation cookie")
	return rec, m[1], cookie
}

// continueWith posts the confirmation form, optionally with a cookie.
func (f *browserFixture) continueWith(state string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/cli/browser/continue", strings.NewReader(url.Values{"state": {state}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.10:4444"
	if cookie != nil {
		req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	rec := httptest.NewRecorder()
	f.s.Router().ServeHTTP(rec, req)
	return rec
}

// confirmedState runs start and continue (as the same browser) and returns the
// state the browser carries to the IdP and the callback cookie it holds.
func (f *browserFixture) confirmedState(t *testing.T) (string, *http.Cookie) {
	t.Helper()
	_, state, cookie := f.startPage(t, cliLoopback)
	rec := f.continueWith(state, cookie)
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	return state, callbackCookieOf(t, rec)
}

// callbackCookieOf returns the browser-bound callback cookie set by continue.
func callbackCookieOf(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == cliCallbackCookie {
			return c
		}
	}
	require.FailNow(t, "continue must set the callback cookie")
	return nil
}

// callbackWith requests the IdP callback URL, optionally with the callback cookie.
func (f *browserFixture) callbackWith(target string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "192.0.2.10:4444"
	if cookie != nil {
		req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	rec := httptest.NewRecorder()
	f.s.Router().ServeHTTP(rec, req)
	return rec
}

func (f *browserFixture) idpTokenCalls() int {
	f.idp.mu.Lock()
	defer f.idp.mu.Unlock()
	return len(f.idp.tokenForms)
}

// login runs start, continue and the IdP callback and returns the one-time
// code the callback handed to the loopback redirect.
func (f *browserFixture) login(t *testing.T) string {
	t.Helper()
	state, cb := f.confirmedState(t)
	rec2 := f.callbackWith(callbackForState(state), cb)
	require.Equal(t, http.StatusFound, rec2.Code, rec2.Body.String())
	for _, c := range rec2.Result().Cookies() {
		assert.Equal(t, cliCallbackCookie, c.Name, "a CLI login must not create a web session")
		assert.Negative(t, c.MaxAge, "the callback cookie is cleared")
	}
	back, err := url.Parse(rec2.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:5555", back.Host)
	assert.Equal(t, "/callback", back.Path)
	code := back.Query().Get("code")
	require.NotEmpty(t, code)
	assert.Equal(t, "no-store", rec2.Header().Get("Cache-Control"))
	return code
}

func TestCLIBrowser_RedirectValidation(t *testing.T) {
	f := newBrowserFixture(t, true)
	ch := pkceChallenge(cliVerifier)
	bad := []string{
		"http://127.0.0.1.evil.com:1234/callback",
		"http://localhost:1234/callback",
		"https://127.0.0.1:1234/callback",
		"http://127.0.0.1/callback",
		"http://user@127.0.0.1:1/callback",
		"http://127.0.0.1:1/callback?x=1",
		"http://127.0.0.1:1/callback?",
		"http://127.0.0.1:1/callback#frag",
		"http://127.0.0.1:1/callback/",
		"http://127.0.0.1:1/other",
		"http://127.0.0.1:1/callback/../callback",
		"http://127.0.0.1:0/callback",
		"http://127.0.0.1:99999/callback",
		"http://127.0.0.1:abc/callback",
		"http://[::1]/callback",
		"http://[::2]:1/callback",
		"http://127.0.0.2:1/callback",
		"//127.0.0.1:1/callback",
		"http://127.0.0.1:1\\@evil.com/callback",
		"http://127.0.0.1:1/callback ",
		"",
	}
	for _, r := range bad {
		rec := f.do(http.MethodGet, startURL(r, ch), nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "redirect %q", r)
	}
	for _, r := range []string{"http://127.0.0.1:5555/callback", "http://[::1]:5555/callback"} {
		rec := f.do(http.MethodGet, startURL(r, ch), nil)
		assert.Equal(t, http.StatusOK, rec.Code, "redirect %q: %s", r, rec.Body.String())
	}
}

func TestCLIBrowser_ChallengeValidation(t *testing.T) {
	f := newBrowserFixture(t, true)
	for _, ch := range []string{"", "short", strings.Repeat("!", 43), pkceChallenge(cliVerifier) + "="} {
		rec := f.do(http.MethodGet, startURL(cliLoopback, ch), nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "challenge %q", ch)
	}
}

func TestCLIBrowser_StartUnavailable(t *testing.T) {
	f := newBrowserFixture(t, false)
	rec := f.do(http.MethodGet, startURL(cliLoopback, pkceChallenge(cliVerifier)), nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "browser login requires the controller key manager", strings.TrimSpace(rec.Body.String()))

	f = newBrowserFixture(t, true)
	f.s.oidcCfg.ClientSecret = ""
	rec = f.do(http.MethodGet, startURL(cliLoopback, pkceChallenge(cliVerifier)), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, http.StatusNotFound, f.exchange("x", cliVerifier).Code)
}

func TestCLIBrowser_StartShowsConfirmationPage(t *testing.T) {
	f := newBrowserFixture(t, true)
	rec, state, cookie := f.startPage(t, cliLoopback)
	assert.Empty(t, rec.Header().Get("Location"), "start must not redirect to the IdP")
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	body := rec.Body.String()
	assert.Contains(t, body, "(port 5555)")
	assert.Contains(t, body, "Sign in unified-cli on this computer (port 5555)? Only continue if you just ran")
	assert.Contains(t, body, "unified-cli login --browser")
	assert.Contains(t, body, "Continue")
	assert.Contains(t, body, "Cancel")
	assert.Contains(t, body, `action="/api/v1/auth/cli/browser/continue"`)
	assert.NotEmpty(t, state)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")

	assert.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
	assert.True(t, cookie.HttpOnly)
	assert.True(t, cookie.Secure, "Secure unless InsecureCookies")
	assert.Equal(t, "/api/v1/auth/cli/browser/", cookie.Path)
	assert.Equal(t, int(stateTTL.Seconds()), cookie.MaxAge)
	assert.Len(t, cookie.Value, 43, "32 random bytes, base64url")
	assert.NotContains(t, body, cookie.Value, "the nonce must never appear in the page")
	assert.NotContains(t, body, "idToken")

	// The loopback host is shown as the port only; an IPv6 loopback works too.
	rec6, _, _ := f.startPage(t, "http://[::1]:6006/callback")
	assert.Contains(t, rec6.Body.String(), "(port 6006)")
}

func TestCLIBrowser_StartCookieNotSecureWithInsecureCookies(t *testing.T) {
	f := newBrowserFixture(t, true)
	f.s.cfg.InsecureCookies = true
	_, _, cookie := f.startPage(t, cliLoopback)
	assert.False(t, cookie.Secure)
	assert.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
}

func TestCLIBrowser_ContinueRequiresTheStartingBrowser(t *testing.T) {
	f := newBrowserFixture(t, true)
	_, state, cookie := f.startPage(t, cliLoopback)
	_, _, otherCookie := f.startPage(t, cliLoopback)
	const want = "confirmation required: open the link printed by unified-cli login --browser on this computer"

	rec := f.continueWith(state, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "no cookie")
	assert.Equal(t, want, strings.TrimSpace(rec.Body.String()))
	assert.Empty(t, rec.Header().Get("Location"))

	rec = f.continueWith(state, otherCookie)
	assert.Equal(t, http.StatusForbidden, rec.Code, "cookie of a different start")
	assert.Equal(t, want, strings.TrimSpace(rec.Body.String()))

	rec = f.continueWith(state, &http.Cookie{Name: cookie.Name, Value: "x"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "wrong nonce")

	assert.Equal(t, http.StatusBadRequest, f.continueWith("unknown-state", cookie).Code)

	// The failed attempts did not consume the state.
	rec = f.continueWith(state, cookie)
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	loc, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, state, loc.Query().Get("state"))
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == cliConfirmCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	assert.True(t, cleared, "continue clears the cookie")
}

func TestCLIBrowser_ContinueErrorsCarryPageHeaders(t *testing.T) {
	f := newBrowserFixture(t, true)
	_, state, _ := f.startPage(t, cliLoopback)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"403": f.continueWith(state, nil),
		"400": f.continueWith("unknown-state", nil),
	} {
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"), name)
		assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"), name)
		assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"), name)
		assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'", name)
	}
}

func TestCLIBrowser_ContinueRateLimit(t *testing.T) {
	f := newBrowserFixture(t, true)
	var last int
	for i := 0; i < 25; i++ {
		last = f.continueWith("x", nil).Code
	}
	assert.Equal(t, http.StatusTooManyRequests, last)
}

func TestCLIBrowser_FullFlow(t *testing.T) {
	f := newBrowserFixture(t, true)
	_, state, cookie := f.startPage(t, cliLoopback)
	rec := f.continueWith(state, cookie)
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	loc, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(rec.Header().Get("Location"), f.idp.URL+"/auth?"))
	assert.Equal(t, "offline", loc.Query().Get("access_type"), "authParams apply to the CLI redirect")
	assert.Equal(t, "web", loc.Query().Get("client_id"))
	assert.Equal(t, "code", loc.Query().Get("response_type"))
	assert.Equal(t, state, loc.Query().Get("state"))

	cb := callbackCookieOf(t, rec)
	assert.True(t, cb.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, cb.SameSite)
	assert.Equal(t, cliCallbackPath, cb.Path)
	assert.Greater(t, cb.MaxAge, 0)
	rec2 := f.callbackWith(callbackForState(state), cb)
	require.Equal(t, http.StatusFound, rec2.Code, rec2.Body.String())
	for _, c := range rec2.Result().Cookies() {
		assert.Equal(t, cliCallbackCookie, c.Name, "a CLI login must not create a web session")
	}
	back, err := url.Parse(rec2.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:5555", back.Host)
	assert.Equal(t, "/callback", back.Path)
	code := back.Query().Get("code")
	require.NotEmpty(t, code)
	assert.Equal(t, int32(1), f.st.created.Load())

	rec = f.exchange(code, cliVerifier)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got api.CLITokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.NotEmpty(t, got.IDToken)
	assert.Equal(t, "rt-browser", got.RefreshToken)
	assert.True(t, got.Expiry.Equal(f.exp), "expiry %v want %v", got.Expiry, f.exp)
	claims, err := f.s.verifyBearerClaims(t.Context(), got.IDToken)
	require.NoError(t, err)
	assert.Equal(t, "u1", claims["sub"])

	rec = f.exchange(code, cliVerifier)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"invalid_grant"}`, rec.Body.String())
}

// An attacker drives start and continue with its own cookie jar and hands the
// IdP link to a victim: the victim's browser has no callback cookie, so the
// callback must refuse and must not reach the loopback or the IdP.
func TestCLIBrowser_CallbackRequiresTheConfirmingBrowser(t *testing.T) {
	const want = "confirmation required: open the link printed by unified-cli login --browser on this computer"
	check := func(t *testing.T, f *browserFixture, rec *httptest.ResponseRecorder) {
		t.Helper()
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Equal(t, want, strings.TrimSpace(rec.Body.String()))
		assert.Empty(t, rec.Header().Get("Location"), "no redirect to the loopback, not even with an error")
		assert.Equal(t, int32(0), f.st.created.Load(), "no code is issued")
		assert.Equal(t, 0, f.idpTokenCalls(), "the IdP code is not exchanged")
	}

	t.Run("no cookie", func(t *testing.T) {
		f := newBrowserFixture(t, true)
		state, _ := f.confirmedState(t)
		check(t, f, f.callbackWith(callbackForState(state), nil))
	})
	t.Run("cookie of a different login", func(t *testing.T) {
		f := newBrowserFixture(t, true)
		state, _ := f.confirmedState(t)
		_, otherCB := f.confirmedState(t)
		check(t, f, f.callbackWith(callbackForState(state), otherCB))
	})
	t.Run("confirm cookie is not a callback cookie", func(t *testing.T) {
		f := newBrowserFixture(t, true)
		_, state, confirm := f.startPage(t, cliLoopback)
		require.Equal(t, http.StatusFound, f.continueWith(state, confirm).Code)
		check(t, f, f.callbackWith(callbackForState(state), &http.Cookie{Name: cliCallbackCookie, Value: confirm.Value}))
	})
	t.Run("state never confirmed", func(t *testing.T) {
		f := newBrowserFixture(t, true)
		_, state, _ := f.startPage(t, cliLoopback)
		check(t, f, f.callbackWith(callbackForState(state), &http.Cookie{Name: cliCallbackCookie, Value: "x"}))
	})
	t.Run("IdP error without cookie", func(t *testing.T) {
		f := newBrowserFixture(t, true)
		state, _ := f.confirmedState(t)
		check(t, f, f.callbackWith("/api/v1/auth/oidc-callback?state="+url.QueryEscape(state)+"&error=access_denied", nil))
	})
	t.Run("refusal clears the cookie and burns the state", func(t *testing.T) {
		f := newBrowserFixture(t, true)
		state, cb := f.confirmedState(t)
		rec := f.callbackWith(callbackForState(state), nil)
		var cleared bool
		for _, c := range rec.Result().Cookies() {
			if c.Name == cliCallbackCookie && c.MaxAge < 0 {
				cleared = true
			}
		}
		assert.True(t, cleared)
		assert.Equal(t, http.StatusBadRequest, f.callbackWith(callbackForState(state), cb).Code)
	})
}

func TestCLIBrowser_WrongVerifierBurnsTheCode(t *testing.T) {
	f := newBrowserFixture(t, true)
	code := f.login(t)
	rec := f.exchange(code, cliVerifier+"x")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"invalid_grant"}`, rec.Body.String())
	rec = f.exchange(code, cliVerifier)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "the row was deleted by the failed attempt")
}

func TestCLIBrowser_ExpiredAndUnknownCodes(t *testing.T) {
	f := newBrowserFixture(t, true)
	// An expired row, as if redeemed after the 2 minute TTL.
	_, err := f.st.Store.CreateCLILoginCode(t.Context(), hashCLICode("expired-code"), pkceChallenge(cliVerifier), []byte("d"), []byte("c"), time.Now().Add(-time.Second))
	require.NoError(t, err)
	rec := f.exchange("expired-code", cliVerifier)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"invalid_grant"}`, rec.Body.String())

	rec = f.exchange("never-issued", cliVerifier)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"invalid_grant"}`, rec.Body.String())

	assert.Equal(t, http.StatusBadRequest, f.exchange("", "").Code)
}

func TestCLIBrowser_CodeTTLIsTwoMinutes(t *testing.T) {
	assert.Equal(t, 2*time.Minute, cliLoginCodeTTL)
}

func TestCLIBrowser_AdmissionDeniedRedirectsWithAccessDenied(t *testing.T) {
	f := newBrowserFixture(t, true)
	f.groups = []string{"nobody"}
	state, cb := f.confirmedState(t)
	rec := f.callbackWith(callbackForState(state), cb)
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	assert.Equal(t, "http://127.0.0.1:5555/callback?error=access_denied", rec.Header().Get("Location"))
	assert.Equal(t, int32(0), f.st.created.Load(), "no code is issued")
	for _, c := range rec.Result().Cookies() {
		assert.Equal(t, cliCallbackCookie, c.Name)
	}
}

func TestCLIBrowser_WebAdmissionDeniedKeepsErrorPage(t *testing.T) {
	f := newBrowserFixture(t, true)
	f.groups = []string{"nobody"}
	start := f.do(http.MethodGet, "/api/v1/auth/oidc-login", nil)
	require.Equal(t, http.StatusFound, start.Code)
	rec := f.do(http.MethodGet, callbackURL(start), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "forbidden: no role assigned")
	assert.Empty(t, rec.Header().Get("Location"))
}

func TestCLIBrowser_IdPErrorIsHandedToTheLoopback(t *testing.T) {
	f := newBrowserFixture(t, true)
	for _, tc := range []struct{ idp, want string }{
		{"access_denied", "access_denied"},
		{"login_required", "login_required"},
		{"Bad Code!&x=1", "server_error"},
	} {
		state, cb := f.confirmedState(t)
		rec := f.callbackWith("/api/v1/auth/oidc-callback?state="+url.QueryEscape(state)+"&error="+url.QueryEscape(tc.idp), cb)
		require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
		assert.Equal(t, "http://127.0.0.1:5555/callback?error="+tc.want, rec.Header().Get("Location"))
		// The state is consumed.
		assert.Equal(t, http.StatusBadRequest, f.do(http.MethodGet, callbackForState(state), nil).Code)
	}
	assert.Equal(t, int32(0), f.st.created.Load())
}

func TestCLIBrowser_WebIdPErrorKeepsErrorPage(t *testing.T) {
	f := newBrowserFixture(t, true)
	start := f.do(http.MethodGet, "/api/v1/auth/oidc-login", nil)
	loc, _ := url.Parse(start.Header().Get("Location"))
	rec := f.do(http.MethodGet, "/api/v1/auth/oidc-callback?state="+url.QueryEscape(loc.Query().Get("state"))+"&error=access_denied", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, rec.Header().Get("Location"))
}

func TestCLIBrowser_StateIsSingleUse(t *testing.T) {
	f := newBrowserFixture(t, true)
	state, cb := f.confirmedState(t)
	target := callbackForState(state)
	assert.Equal(t, http.StatusFound, f.callbackWith(target, cb).Code)
	assert.Equal(t, http.StatusBadRequest, f.callbackWith(target, cb).Code)
	assert.Equal(t, int32(1), f.st.created.Load())
}

func TestCLIBrowser_WebLoginUnchanged(t *testing.T) {
	f := newBrowserFixture(t, true)
	start := f.do(http.MethodGet, "/api/v1/auth/oidc-login", nil)
	require.Equal(t, http.StatusFound, start.Code)
	rec := f.do(http.MethodGet, callbackURL(start), nil)
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	assert.Equal(t, "/ui/", rec.Header().Get("Location"))
	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			found = true
		}
	}
	assert.True(t, found, "web login still sets the session cookie")
	assert.Equal(t, int32(0), f.st.created.Load())
}

func TestCLIBrowser_ExchangeRateLimit(t *testing.T) {
	f := newBrowserFixture(t, true)
	var last int
	for i := 0; i < 25; i++ {
		last = f.exchange("x", cliVerifier).Code
	}
	assert.Equal(t, http.StatusTooManyRequests, last)
}
