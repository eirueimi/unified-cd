package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/store"
)

// adminOnly runs the request through ServerAuth + an admin-only gate and
// returns the status code and the role seen by the final handler.
func adminOnly(t *testing.T, st store.Store, srv *Server, token string) (int, string) {
	t.Helper()
	var role string
	h := ServerAuth(st, srv)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalFromContext(r.Context())
		role = p.Role
		if p.Role != "admin" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, role
}

func TestServerAuth_NameImpersonationIsNotBreakGlass(t *testing.T) {
	pg := store.NewTestPostgres(t)
	s := NewServer(Config{Token: "real-bootstrap"}, pg)
	t.Cleanup(s.Close)

	_, err := pg.CreatePAT(t.Context(), BootstrapPATName, HashToken("attacker-token"), "developer", nil)
	require.NoError(t, err)

	code, _ := adminOnly(t, pg, s, "attacker-token")
	assert.Equal(t, http.StatusForbidden, code, "same-named developer PAT must not be admin")
}

func TestServerAuth_RealBootstrapTokenIsAdmin(t *testing.T) {
	pg := store.NewTestPostgres(t)
	s := NewServer(Config{Token: "real-bootstrap"}, pg)
	t.Cleanup(s.Close)

	// Stored role deliberately "developer": break-glass comes from the hash match.
	_, err := pg.CreatePAT(t.Context(), BootstrapPATName, HashToken("real-bootstrap"), "developer", nil)
	require.NoError(t, err)

	code, role := adminOnly(t, pg, s, "real-bootstrap")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "admin", role)
}

func TestServerAuth_NoConfiguredTokenNoBreakGlass(t *testing.T) {
	pg := store.NewTestPostgres(t)
	_, err := pg.CreatePAT(t.Context(), BootstrapPATName, HashToken("some-token"), "developer", nil)
	require.NoError(t, err)

	code, _ := adminOnly(t, pg, nil, "some-token")
	assert.Equal(t, http.StatusForbidden, code, "ServerAuth(st, nil) has no break-glass")

	s := NewServer(Config{}, pg)
	t.Cleanup(s.Close)
	code, _ = adminOnly(t, pg, s, "some-token")
	assert.Equal(t, http.StatusForbidden, code, "empty Config.Token has no break-glass")
}

func TestCreateToken_ReservedNames(t *testing.T) {
	s, _ := newTestServer(t)
	for _, name := range []string{"env:UNIFIED_TOKEN", "env:x"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", bytes.NewReader([]byte(`{"name":"`+name+`"}`)))
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, name)
		assert.Equal(t, "token name \""+name+"\" is reserved\n", rec.Body.String())
	}
}
