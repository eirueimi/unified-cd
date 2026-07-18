package vault

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVault is a minimal Transit-speaking server: it base64s on encrypt and
// reverses that on decrypt, which is enough to verify the wire contract.
func fakeVault(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token/lookup-self", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": map[string]any{"ttl": 3600, "renewable": true}})
	})
	mux.HandleFunc("/v1/auth/token/renew-self", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"auth": map[string]any{"lease_duration": 3600, "renewable": true}})
	})
	mux.HandleFunc("/v1/transit/encrypt/", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Plaintext string `json:"plaintext"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		writeJSON(w, map[string]any{"data": map[string]any{"ciphertext": "vault:v1:" + req.Plaintext}})
	})
	mux.HandleFunc("/v1/transit/decrypt/", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Ciphertext string `json:"ciphertext"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		writeJSON(w, map[string]any{"data": map[string]any{
			"plaintext": strings.TrimPrefix(req.Ciphertext, "vault:v1:"),
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newTestKeyManager(t *testing.T, addr string) *KeyManager {
	t.Helper()
	m, err := New(context.Background(), Config{
		Address: addr, Mount: "transit", Key: "unified-cd-kek",
		Auth: "token", Token: "s.test",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestKeyManager_RoundTrip(t *testing.T) {
	m := newTestKeyManager(t, fakeVault(t).URL)
	dek := []byte("0123456789abcdef0123456789abcdef")

	wrapped, err := m.EncryptKey(context.Background(), dek)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(wrapped), "vault:"),
		"Transit ciphertext is already self-describing")

	got, err := m.DecryptKey(context.Background(), wrapped)
	require.NoError(t, err)
	assert.Equal(t, dek, got)
}

// Transit ciphertext already begins with "vault:". Adding a provider prefix of
// our own would produce "vault:vault:v1:".
func TestKeyManager_DoesNotDoublePrefix(t *testing.T) {
	m := newTestKeyManager(t, fakeVault(t).URL)
	wrapped, err := m.EncryptKey(context.Background(), []byte("dek"))
	require.NoError(t, err)
	assert.False(t, strings.HasPrefix(string(wrapped), "vault:vault:"))
}

// The mirror of LocalKeyManager.DecryptKey: data wrapped by the other provider
// must report precisely, not as an opaque decrypt failure.
func TestKeyManager_RejectsForeignProvider(t *testing.T) {
	m := newTestKeyManager(t, fakeVault(t).URL)
	_, err := m.DecryptKey(context.Background(), []byte("local:deadbeef"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, secrets.ErrProviderMismatch), "got %v", err)
	assert.Contains(t, err.Error(), "hashivault")
}

func TestKeyManager_UnreachableAddressFailsFast(t *testing.T) {
	_, err := New(context.Background(), Config{
		Address: "http://127.0.0.1:1", Mount: "transit", Key: "k",
		Auth: "token", Token: "s.test",
	})
	require.Error(t, err)
}

// Transit works on base64; a DEK is arbitrary bytes and must survive intact.
func TestKeyManager_HandlesArbitraryBytes(t *testing.T) {
	m := newTestKeyManager(t, fakeVault(t).URL)
	dek := []byte{0x00, 0xff, 0x10, 0x00, 0x7f}

	wrapped, err := m.EncryptKey(context.Background(), dek)
	require.NoError(t, err)
	got, err := m.DecryptKey(context.Background(), wrapped)
	require.NoError(t, err)
	assert.Equal(t, dek, got)
	_ = base64.StdEncoding
}

// A second decrypt of the same wrapped DEK must not call Transit again, and
// must still return usable bytes after the first caller zeroed its copy.
func TestKeyManager_CachesUnwrappedDEKs(t *testing.T) {
	var decrypts atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token/lookup-self", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": map[string]any{"ttl": 3600, "renewable": true}})
	})
	mux.HandleFunc("/v1/transit/decrypt/", func(w http.ResponseWriter, r *http.Request) {
		decrypts.Add(1)
		var req struct {
			Ciphertext string `json:"ciphertext"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		writeJSON(w, map[string]any{"data": map[string]any{
			"plaintext": strings.TrimPrefix(req.Ciphertext, "vault:v1:"),
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := newTestKeyManager(t, srv.URL)
	wrapped := []byte("vault:v1:" + base64.StdEncoding.EncodeToString([]byte("dek")))

	first, err := m.DecryptKey(context.Background(), wrapped)
	require.NoError(t, err)
	for i := range first {
		first[i] = 0 // the caller's zeroing defer
	}

	second, err := m.DecryptKey(context.Background(), wrapped)
	require.NoError(t, err)
	assert.Equal(t, []byte("dek"), second)
	assert.EqualValues(t, 1, decrypts.Load(), "the second decrypt must be served from cache")
}
