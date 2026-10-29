package controller

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const authzTestIssuer = "https://idp.example.com"

// newAuthzTestServer builds a KM-enabled test server with OIDC configured (issuer https://idp.example.com,
// clientId unified-cd, rolesClaim groups), the given legacy maps and permissions YAML compiled via authz.Compile,
// and verifyIDTokenHook accepting raw tokens "idtoken:<email>|<g1,g2>" (claims sub=email, email, groups;
// "idtoken:<email>|-" omits the groups claim).
func newAuthzTestServer(t *testing.T, legacy authz.Legacy, permissionsYAML string) (*Server, store.Store) {
	t.Helper()
	pg := store.NewTestPostgres(t)
	return newAuthzTestServerOn(t, pg, legacy, permissionsYAML), pg
}

// newAuthzTestServerOn is newAuthzTestServer over an existing store, so two
// servers with different configs can share the same rows.
func newAuthzTestServerOn(t *testing.T, pg store.Store, legacy authz.Legacy, permissionsYAML string) *Server {
	t.Helper()
	var cfg *authz.PolicyConfig
	if strings.TrimSpace(permissionsYAML) != "" {
		cfg = &authz.PolicyConfig{}
		require.NoError(t, yaml.Unmarshal([]byte(permissionsYAML), cfg))
	}
	policy, _, err := authz.Compile(cfg, legacy, authzTestIssuer)
	require.NoError(t, err)

	s := NewServer(Config{}, pg)
	s.SetKeyManager(testKeyManager(t))
	s.SetOIDCConfig(&OIDCConfig{
		Issuer:      authzTestIssuer,
		ClientID:    "unified-cd",
		RolesClaim:  "groups",
		RoleMap:     legacy.RoleMap,
		UserMap:     legacy.UserMap,
		DefaultRole: legacy.DefaultRole,
		Policy:      policy,
	})
	s.verifyIDTokenHook = func(_ context.Context, raw, _ string) (map[string]any, error) {
		rest, ok := strings.CutPrefix(raw, "idtoken:")
		if !ok {
			return nil, errors.New("not a test id_token")
		}
		email, groups, _ := strings.Cut(rest, "|")
		claims := map[string]any{"sub": email, "email": email}
		if groups != "-" {
			list := []any{}
			for _, g := range strings.Split(groups, ",") {
				if g != "" {
					list = append(list, g)
				}
			}
			claims["groups"] = list
		}
		return claims, nil
	}
	t.Cleanup(s.Close)
	return s
}

// bearer returns a raw token accepted by the newAuthzTestServer hook. With no
// groups the groups claim is omitted.
func bearer(email string, groups ...string) string {
	if len(groups) == 0 {
		return "idtoken:" + email + "|-"
	}
	return "idtoken:" + email + "|" + strings.Join(groups, ",")
}

// doAs sends a request through the full router with the given bearer token.
func doAs(t *testing.T, s *Server, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", bearerPrefix+token)
	}
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

// authenticate runs ServerAuth in front of a handler that records the
// principal, using the given bearer token and/or session cookie value.
func authenticate(t *testing.T, s *Server, token, sessionToken string) (Principal, *httptest.ResponseRecorder) {
	t.Helper()
	var got Principal
	h := ServerAuth(s.store, s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = principalFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if token != "" {
		req.Header.Set("Authorization", bearerPrefix+token)
	}
	if sessionToken != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return got, rec
}

// addSession stores a session whose cookie value is sessionToken.
func addSession(t *testing.T, st store.Store, sessionToken, sub, email, role string, claims []string) {
	t.Helper()
	_, err := st.CreateSession(context.Background(), HashToken(sessionToken), sub, email, role, claims,
		[]byte("dek"), []byte("ct"), time.Now().Add(time.Hour))
	require.NoError(t, err)
}
