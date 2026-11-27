package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func getMeWith(t *testing.T, s *Server, token, secFetchMode string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", bearerPrefix+token)
	if secFetchMode != "" {
		req.Header.Set("Sec-Fetch-Mode", secFetchMode)
	}
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func TestBrowserPATRefusedUnderSSO(t *testing.T) {
	t.Run("sso without allowTokenEntry refuses a browser PAT", func(t *testing.T) {
		s, pg := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, "")
		makeRolePAT(t, s, pg, "tok-admin", "admin")

		rec := getMeWith(t, s, "tok-admin", "cors")
		require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "personal access tokens are not accepted from the browser while SSO is configured; sign in with SSO")

		// Same PAT without the header (CLI, curl) is still accepted.
		rec = getMeWith(t, s, "tok-admin", "")
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("sso with allowTokenEntry accepts a browser PAT", func(t *testing.T) {
		s, pg := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, "")
		s.cfg.AllowTokenEntry = true
		makeRolePAT(t, s, pg, "tok-admin", "admin")

		rec := getMeWith(t, s, "tok-admin", "cors")
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("without sso a browser PAT is accepted", func(t *testing.T) {
		s, _ := newTestServer(t)
		rec := getMeWith(t, s, "secret", "cors")
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("id_token bearer is unaffected", func(t *testing.T) {
		s, _ := newAuthzTestServer(t, authz.Legacy{DefaultRole: "admin"}, "")
		rec := getMeWith(t, s, bearer("a@example.com"), "cors")
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

	sso, _ := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, "")
	assert.Contains(t, get(sso), `"allowTokenEntry":false`)

	sso.cfg.AllowTokenEntry = true
	assert.Contains(t, get(sso), `"allowTokenEntry":true`)
}
