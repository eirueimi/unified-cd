package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func hardeningOKHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestSecurityHeadersMiddleware_SetsAllThree(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://ucd.local/api/v1/jobs", nil)
	securityHeadersMiddleware(hardeningOKHandler()).ServeHTTP(rec, req)
	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "same-origin", rec.Header().Get("Referrer-Policy"))
}

func originCheckStatus(t *testing.T, s *Server, method, origin, referer string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "http://ucd.local/api/v1/runs", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	s.originCheckMiddleware(hardeningOKHandler()).ServeHTTP(rec, req)
	return rec.Code
}

func TestOriginCheck_SameHostOriginPasses(t *testing.T) {
	assert.Equal(t, http.StatusOK, originCheckStatus(t, &Server{}, http.MethodPost, "http://ucd.local", ""))
}

func TestOriginCheck_SchemeIsNotCompared(t *testing.T) {
	// TLS-terminating proxies make the scheme unreliable; host match is enough.
	assert.Equal(t, http.StatusOK, originCheckStatus(t, &Server{}, http.MethodPost, "https://ucd.local", ""))
}

func TestOriginCheck_CrossOriginRejected(t *testing.T) {
	assert.Equal(t, http.StatusForbidden, originCheckStatus(t, &Server{}, http.MethodPost, "https://evil.example", ""))
}

func TestOriginCheck_PortMismatchRejected(t *testing.T) {
	assert.Equal(t, http.StatusForbidden, originCheckStatus(t, &Server{}, http.MethodPost, "http://ucd.local:8080", ""))
}

func TestOriginCheck_NullOriginRejected(t *testing.T) {
	assert.Equal(t, http.StatusForbidden, originCheckStatus(t, &Server{}, http.MethodPost, "null", ""))
}

func TestOriginCheck_NoHeadersPass(t *testing.T) {
	// CLI / agents / webhooks send neither Origin nor Referer.
	assert.Equal(t, http.StatusOK, originCheckStatus(t, &Server{}, http.MethodPost, "", ""))
}

func TestOriginCheck_RefererFallbackMatchPasses(t *testing.T) {
	assert.Equal(t, http.StatusOK, originCheckStatus(t, &Server{}, http.MethodPost, "", "http://ucd.local/ui/"))
}

func TestOriginCheck_RefererFallbackMismatchRejected(t *testing.T) {
	assert.Equal(t, http.StatusForbidden, originCheckStatus(t, &Server{}, http.MethodPost, "", "https://evil.example/page"))
}

func TestOriginCheck_GetPassesRegardless(t *testing.T) {
	assert.Equal(t, http.StatusOK, originCheckStatus(t, &Server{}, http.MethodGet, "https://evil.example", ""))
}

func TestOriginCheck_ExternalURLHostAllowed(t *testing.T) {
	s := &Server{oidcCfg: &OIDCConfig{ExternalURL: "https://ci.example.com"}}
	assert.Equal(t, http.StatusOK, originCheckStatus(t, s, http.MethodPost, "https://ci.example.com", ""))
}

func TestSessionCookie_SecureByDefault(t *testing.T) {
	s := &Server{cfg: Config{}}
	c := s.sessionCookie("tok", time.Now().Add(time.Hour), 0)
	assert.True(t, c.Secure)
	assert.True(t, c.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, c.SameSite)
	assert.Equal(t, "/", c.Path)
	assert.Equal(t, "ucd_session", c.Name)
	assert.Equal(t, "tok", c.Value)
}

func TestSessionCookie_InsecureCookiesOptOut(t *testing.T) {
	s := &Server{cfg: Config{InsecureCookies: true}}
	assert.False(t, s.sessionCookie("tok", time.Now().Add(time.Hour), 0).Secure)
}

func TestSessionCookie_LogoutDeletionShape(t *testing.T) {
	s := &Server{cfg: Config{}}
	c := s.sessionCookie("", time.Time{}, -1)
	assert.Equal(t, "", c.Value)
	assert.Equal(t, -1, c.MaxAge)
	assert.True(t, c.Secure)
}
