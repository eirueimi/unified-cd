package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
)

func TestAuthorize_UnclassifiedRouteFailsClosed(t *testing.T) {
	s, st := newAuthzTestServer(t, authz.Legacy{}, "")
	mustOwnedPAT(t, st, "unclassified-tok", "admin", nil, []string{"g"})

	reached := false
	router := chi.NewRouter()
	router.With(ServerAuth(s.store, s), s.authorize).
		Get("/api/v1/not-in-the-table", func(http.ResponseWriter, *http.Request) { reached = true })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/not-in-the-table", nil)
	req.Header.Set("Authorization", bearerPrefix+"unclassified-tok")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "forbidden: unclassified route\n", rec.Body.String())
	assert.False(t, reached)
}
