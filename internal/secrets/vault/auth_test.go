package vault

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestStaticTokenAuth_ReadsFile(t *testing.T) {
	a, err := newStaticTokenAuth("", writeTokenFile(t, "s.abc123"))
	require.NoError(t, err)
	got, err := a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.abc123", got.Token)
}

// Editors and `echo` append newlines; a trailing newline must not break startup.
func TestStaticTokenAuth_TrimsWhitespace(t *testing.T) {
	a, err := newStaticTokenAuth("", writeTokenFile(t, "  s.abc123\n"))
	require.NoError(t, err)
	got, err := a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.abc123", got.Token)
}

// The file is re-read on every login, so an operator can replace a rotated
// token without restarting the controller.
func TestStaticTokenAuth_RereadsFileOnEachLogin(t *testing.T) {
	path := writeTokenFile(t, "s.first")
	a, err := newStaticTokenAuth("", path)
	require.NoError(t, err)

	first, err := a.login(context.Background())
	require.NoError(t, err)
	require.Equal(t, "s.first", first.Token)

	require.NoError(t, os.WriteFile(path, []byte("s.second"), 0o600))
	second, err := a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.second", second.Token, "a replaced token file must take effect without a restart")
}

func TestStaticTokenAuth_FilePreferredOverLiteral(t *testing.T) {
	a, err := newStaticTokenAuth("s.from-env", writeTokenFile(t, "s.from-file"))
	require.NoError(t, err)
	got, err := a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.from-file", got.Token,
		"a file is preferred: it does not leak into docker inspect or child processes")
}

func TestStaticTokenAuth_LiteralUsedWhenNoFile(t *testing.T) {
	a, err := newStaticTokenAuth("s.from-env", "")
	require.NoError(t, err)
	got, err := a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.from-env", got.Token)
}

func TestStaticTokenAuth_NeitherIsAnError(t *testing.T) {
	_, err := newStaticTokenAuth("", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNIFIED_VAULT_TOKEN_FILE")
	assert.Contains(t, err.Error(), "VAULT_TOKEN")
}

func TestStaticTokenAuth_MissingFileReportsPath(t *testing.T) {
	a, err := newStaticTokenAuth("", filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err, "a missing file is a login-time failure, not a construction failure")
	_, err = a.login(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent")
}

// newTestVaultClient builds a real *vaultapi.Client pointed at a fake Vault
// server, mirroring what newAuth hands selfLookupAuth in production.
func newTestVaultClient(t *testing.T, addr string) *vaultapi.Client {
	t.Helper()
	cfg := vaultapi.DefaultConfig()
	cfg.Address = addr
	client, err := vaultapi.NewClient(cfg)
	require.NoError(t, err)
	return client
}

// selfLookupAuth's central claim is that it enriches whatever token its inner
// auth produces with the TTL and renewability Vault reports for that token,
// via a real auth/token/lookup-self round trip.
func TestSelfLookupAuth_PopulatesTTLAndRenewableFromLookup(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token/lookup-self", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"ttl": 3600, "renewable": true},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	inner := &fakeAuth{result: authResult{Token: "s.inner-token"}}
	a := &selfLookupAuth{inner: inner, client: newTestVaultClient(t, srv.URL)}

	got, err := a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.inner-token", got.Token, "the wrapped inner auth's token must pass through unchanged")
	assert.Equal(t, time.Hour, got.TTL)
	assert.True(t, got.Renewable)
}

// A tightly-scoped token that was never granted `read` on
// auth/token/lookup-self must not be treated as a login failure: it is a
// degradation (renewal disabled) rather than an outage.
func TestSelfLookupAuth_DeniedLookupDegradesInsteadOfFailing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token/lookup-self", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{"permission denied"}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	inner := &fakeAuth{result: authResult{Token: "s.inner-token"}}
	a := &selfLookupAuth{inner: inner, client: newTestVaultClient(t, srv.URL)}

	got, err := a.login(context.Background())
	require.NoError(t, err, "a denied lookup-self must degrade, not fail login")
	assert.Equal(t, "s.inner-token", got.Token, "the inner token must still be returned")
	assert.Zero(t, got.TTL, "the pre-decorator behaviour (TTL: 0) is the safe fallback")
	assert.False(t, got.Renewable)
}

// isPermissionDenied must distinguish "Vault said no" from "Vault was never
// reached" so an unreachable address stays a hard failure while a denied
// lookup degrades gracefully.
func TestIsPermissionDenied(t *testing.T) {
	assert.True(t, isPermissionDenied(&vaultapi.ResponseError{StatusCode: http.StatusForbidden}))
	assert.False(t, isPermissionDenied(&vaultapi.ResponseError{StatusCode: http.StatusInternalServerError}))
	assert.False(t, isPermissionDenied(errors.New("dial tcp 127.0.0.1:1: connect: connection refused")))
}
