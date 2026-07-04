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
	_, err := pg.CreatePAT(t.Context(), "dev", HashToken("exc_devtoken"), "developer", nil)
	require.NoError(t, err)

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

func TestServerAuth_BootstrapIsAdmin(t *testing.T) {
	s, pg := newTestServer(t)
	_, err := pg.UpsertBootstrapPAT(t.Context(), BootstrapPATName, HashToken("exc_boot"))
	require.NoError(t, err)

	var gotRole string
	h := ServerAuth(s.store, s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalFromContext(r.Context())
		gotRole = p.Role
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer exc_boot")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "admin", gotRole)
}
