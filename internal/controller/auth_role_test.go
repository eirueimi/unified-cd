package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)
func TestServerAuth_SetsPATRole(t *testing.T) {
	s, pg := newTestServer(t)
	addRoleServicePAT(t, s, pg, "dev", "exc_devtoken", "developer")

	var gotRole string
	h := ServerAuth(s.store, s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalFromContext(r.Context())
		gotRole = p.Role
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer exc_devtoken")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "developer", gotRole)
}
