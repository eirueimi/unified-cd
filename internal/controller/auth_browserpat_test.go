package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Header sets as sent by a browser fetch/XHR and by Node's undici fetch.
var (
	browserFetchHeaders = map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors"}
	nodeFetchHeaders    = map[string]string{"Sec-Fetch-Mode": "cors"}
)

func getMeWith(t *testing.T, s *Server, token string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", bearerPrefix+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func TestBrowserPATRefusedUnderSSO(t *testing.T) {
	t.Run("browser SSO without allowTokenEntry refuses a browser PAT", func(t *testing.T) {
		s, pg := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, "")
		s.oidcCfg.ClientSecret = "browser-secret"
		makeRolePAT(t, s, pg, "tok-admin", "admin")

		rec := getMeWith(t, s, "tok-admin", browserFetchHeaders)
		require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "personal access tokens are not accepted from the browser while SSO is configured; sign in with SSO")

		// Same PAT without the headers (CLI, curl) is still accepted.
		rec = getMeWith(t, s, "tok-admin", nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		// Node's fetch sends Sec-Fetch-Mode but no Sec-Fetch-Site: not a browser.
		rec = getMeWith(t, s, "tok-admin", nodeFetchHeaders)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("sso with allowTokenEntry accepts a browser PAT", func(t *testing.T) {
		s, pg := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, "")
		s.oidcCfg.ClientSecret = "browser-secret"
		s.cfg.AllowTokenEntry = true
		makeRolePAT(t, s, pg, "tok-admin", "admin")

		rec := getMeWith(t, s, "tok-admin", browserFetchHeaders)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("sso without a browser client secret accepts a browser PAT", func(t *testing.T) {
		s, pg := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, "")
		makeRolePAT(t, s, pg, "tok-admin", "admin")

		rec := getMeWith(t, s, "tok-admin", browserFetchHeaders)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("without sso a browser PAT is accepted", func(t *testing.T) {
		s, _ := newTestServer(t)
		rec := getMeWith(t, s, "secret", browserFetchHeaders)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("id_token bearer is unaffected", func(t *testing.T) {
		s, _ := newAuthzTestServer(t, authz.Legacy{DefaultRole: "admin"}, "")
		rec := getMeWith(t, s, bearer("a@example.com"), browserFetchHeaders)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}

func TestUIConfigAllowTokenEntry(t *testing.T) {
	get := func(s *Server) string {
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ui-config", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		return rec.Body.String()
	}

	s, _ := newTestServer(t)
	assert.Contains(t, get(s), `"allowTokenEntry":true`)

	// OIDC without a browser client secret (CLI/device-only SSO): the UI has no
	// SSO sign-in, so the token form must stay.
	sso, _ := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, "")
	assert.Contains(t, get(sso), `"allowTokenEntry":true`)

	sso.oidcCfg.ClientSecret = "browser-secret"
	assert.Contains(t, get(sso), `"allowTokenEntry":false`)

	sso.cfg.AllowTokenEntry = true
	assert.Contains(t, get(sso), `"allowTokenEntry":true`)
}
