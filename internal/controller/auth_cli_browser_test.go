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

func callbackURL(startRec *httptest.ResponseRecorder) string {
	loc, _ := url.Parse(startRec.Header().Get("Location"))
	return "/api/v1/auth/oidc-callback?state=" + url.QueryEscape(loc.Query().Get("state")) + "&code=idp-code"
}

// login runs start and the IdP callback and returns the one-time code the
// callback handed to the loopback redirect.
func (f *browserFixture) login(t *testing.T) string {
	t.Helper()
	rec := f.do(http.MethodGet, startURL(cliLoopback, pkceChallenge(cliVerifier)), nil)
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	rec2 := f.do(http.MethodGet, callbackURL(rec), nil)
	require.Equal(t, http.StatusFound, rec2.Code, rec2.Body.String())
	assert.Empty(t, rec2.Result().Cookies(), "a CLI login must not create a web session")
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
		assert.Equal(t, http.StatusFound, rec.Code, "redirect %q: %s", r, rec.Body.String())
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

func TestCLIBrowser_FullFlow(t *testing.T) {
	f := newBrowserFixture(t, true)
	rec := f.do(http.MethodGet, startURL(cliLoopback, pkceChallenge(cliVerifier)), nil)
	require.Equal(t, http.StatusFound, rec.Code)
	loc, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(rec.Header().Get("Location"), f.idp.URL+"/auth?"))
	assert.Equal(t, "offline", loc.Query().Get("access_type"), "authParams apply to the CLI redirect")
	assert.Equal(t, "web", loc.Query().Get("client_id"))
	assert.Equal(t, "code", loc.Query().Get("response_type"))

	code := f.login(t)
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

func TestCLIBrowser_AdmissionDeniedIssuesNoCode(t *testing.T) {
	f := newBrowserFixture(t, true)
	f.groups = []string{"nobody"}
	start := f.do(http.MethodGet, startURL(cliLoopback, pkceChallenge(cliVerifier)), nil)
	require.Equal(t, http.StatusFound, start.Code)
	rec := f.do(http.MethodGet, callbackURL(start), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "forbidden: no role assigned")
	assert.Empty(t, rec.Header().Get("Location"))
	assert.Equal(t, int32(0), f.st.created.Load())
}

func TestCLIBrowser_StateIsSingleUse(t *testing.T) {
	f := newBrowserFixture(t, true)
	start := f.do(http.MethodGet, startURL(cliLoopback, pkceChallenge(cliVerifier)), nil)
	cb := callbackURL(start)
	assert.Equal(t, http.StatusFound, f.do(http.MethodGet, cb, nil).Code)
	assert.Equal(t, http.StatusBadRequest, f.do(http.MethodGet, cb, nil).Code)
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
