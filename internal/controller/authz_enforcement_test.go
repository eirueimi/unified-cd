package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/require"
)

func makeRolePAT(t *testing.T, s *Server, pg store.Store, token, role string) {
	t.Helper()
	addRoleServicePAT(t, s, pg, role+"-user", token, role)
}

func doReq(t *testing.T, s *Server, method, path, token string, body []byte) int {
	t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, r)
	return rec.Code
}

func TestEnforcement_ViewerCannotWrite(t *testing.T) {
	s, pg := newTestServer(t)
	makeRolePAT(t, s, pg, "exc_view", "viewer")
	require.Equal(t, http.StatusForbidden, doReq(t, s, http.MethodPost, "/api/v1/jobs", "exc_view", []byte(`{}`)))
}

func TestEnforcement_DeveloperCannotSecretsWrite(t *testing.T) {
	s, pg := newTestServer(t)
	makeRolePAT(t, s, pg, "exc_dev", "developer")
	require.Equal(t, http.StatusForbidden, doReq(t, s, http.MethodPost, "/api/v1/secrets/", "exc_dev", []byte(`{}`)))
}

func TestEnforcement_AdminCanSecretsWrite(t *testing.T) {
	s, pg := newTestServer(t)
	makeRolePAT(t, s, pg, "exc_admin", "admin")
	code := doReq(t, s, http.MethodPost, "/api/v1/secrets/", "exc_admin", []byte(`{"name":"X","value":"y"}`))
	require.NotEqual(t, http.StatusForbidden, code) // 200/400/501 ok; must not be 403
}

func TestEnforcement_DeveloperCanTrigger(t *testing.T) {
	s, pg := newTestServer(t)
	makeRolePAT(t, s, pg, "exc_dev2", "developer")
	code := doReq(t, s, http.MethodPost, "/api/v1/runs", "exc_dev2", []byte(`{"job":"nope"}`))
	require.NotEqual(t, http.StatusForbidden, code) // not blocked by RBAC (may 400/404)
}
