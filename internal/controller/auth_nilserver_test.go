package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// ServerAuth without a Server cannot resolve an account, so a valid PAT is
// refused instead of being granted anything.
func TestServerAuth_NilServerRefusesPATs(t *testing.T) {
	_, pg := newTestServer(t)
	h := ServerAuth(pg, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "forbidden: account disabled\n", rec.Body.String())
}
