package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerAuth_StoresPATPrincipal(t *testing.T) {
	s, pg := newTestServer(t)

	// Create a PAT named "ci-bot" with a known plain token.
	plain := "test-pat-token-ci-bot"
	addRoleServicePAT(t, s, pg, "ci-bot", plain, "admin")

	var got Principal
	var ok bool
	h := ServerAuth(pg, s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = principalFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/v1/runs/x", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	require.True(t, ok, "principal must be in context")
	assert.Equal(t, "ci-bot", got.Name)
	assert.Equal(t, "pat", got.Kind)
}
