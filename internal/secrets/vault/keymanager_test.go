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
	mux.HandleFunc("/v1/transit/keys/", func(w http.ResponseWriter, r *http.Request) {
		writeExistingKey(w, r)
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

// writeExistingKey answers a transit/keys/<name> read as if the key exists,
// which is what every fake-server test needs unless it is specifically
// exercising probeKeyExists's own error paths.
func writeExistingKey(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/v1/transit/keys/")
	writeJSON(w, map[string]any{"data": map[string]any{"name": name, "type": "aes256-gcm96"}})
}

// writeMissingKey answers a transit/keys/<name> read the way Vault does when
// the key does not exist: 404 with an empty errors list. The client library
// treats this specially — ParseRawResponseAndCloseBody swallows a 404 into
// (nil, nil) rather than surfacing a *vaultapi.ResponseError — which is
// exactly the case probeKeyExists must still detect.
func writeMissingKey(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{}})
}

// writeKeyReadDenied answers a transit/keys/<name> read the way Vault does
// when the policy lacks `read` on that path.
func writeKeyReadDenied(w http.ResponseWriter) {
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{"permission denied"}})
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
	// New's startup probe reads the key's metadata and then performs one
	// encrypt round trip; both must be wired up even though the test itself
	// only exercises decrypt caching.
	mux.HandleFunc("/v1/transit/keys/", func(w http.ResponseWriter, r *http.Request) {
		writeExistingKey(w, r)
	})
	mux.HandleFunc("/v1/transit/encrypt/", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Plaintext string `json:"plaintext"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		writeJSON(w, map[string]any{"data": map[string]any{"ciphertext": "vault:v1:" + req.Plaintext}})
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

// Design §5: "Token expired or revoked — re-login, then retry the operation
// once." This is the flow docs/secrets.md documents: an operator revokes the
// controller's token and drops a replacement into UNIFIED_VAULT_TOKEN_FILE.
// The very next encrypt must succeed via one re-login and one retry, not fail
// until the next renewal tick (up to half a lease away).
//
// failNext is armed only after construction succeeds, so New's own startup
// probe (which goes through this identical retry path) is not itself part of
// what is being counted here.
func TestKeyManager_Write_RetriesOnceOn403ThenSucceeds(t *testing.T) {
	var encryptCalls atomic.Int64
	var failNext atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token/lookup-self", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": map[string]any{"ttl": 3600, "renewable": true}})
	})
	mux.HandleFunc("/v1/transit/keys/", func(w http.ResponseWriter, r *http.Request) {
		writeExistingKey(w, r)
	})
	mux.HandleFunc("/v1/transit/encrypt/", func(w http.ResponseWriter, r *http.Request) {
		encryptCalls.Add(1)
		if failNext.CompareAndSwap(true, false) {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{"permission denied"}})
			return
		}
		var req struct {
			Plaintext string `json:"plaintext"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		writeJSON(w, map[string]any{"data": map[string]any{"ciphertext": "vault:v1:" + req.Plaintext}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := newTestKeyManager(t, srv.URL) // startup probe succeeds: failNext is not armed yet

	encryptCalls.Store(0)
	failNext.Store(true)
	wrapped, err := m.EncryptKey(context.Background(), []byte("dek"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(wrapped), "vault:"))
	assert.EqualValues(t, 2, encryptCalls.Load(), "one 403 (which triggers a re-login) then one successful retry")
}

// A persistent 403 (a genuinely under-scoped policy, not a stale token) must
// fail after exactly one retry — not retry forever, and not retry a second
// time.
func TestKeyManager_Write_PersistentDenialFailsAfterExactlyOneRetry(t *testing.T) {
	var encryptCalls atomic.Int64
	var alwaysFail atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token/lookup-self", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": map[string]any{"ttl": 3600, "renewable": true}})
	})
	mux.HandleFunc("/v1/transit/keys/", func(w http.ResponseWriter, r *http.Request) {
		writeExistingKey(w, r)
	})
	mux.HandleFunc("/v1/transit/encrypt/", func(w http.ResponseWriter, r *http.Request) {
		encryptCalls.Add(1)
		if alwaysFail.Load() {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{"permission denied"}})
			return
		}
		var req struct {
			Plaintext string `json:"plaintext"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		writeJSON(w, map[string]any{"data": map[string]any{"ciphertext": "vault:v1:" + req.Plaintext}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := newTestKeyManager(t, srv.URL) // startup probe succeeds: alwaysFail is not armed yet

	encryptCalls.Store(0)
	alwaysFail.Store(true)
	_, err := m.EncryptKey(context.Background(), []byte("dek"))
	require.Error(t, err)
	assert.EqualValues(t, 2, encryptCalls.Load(), "exactly one retry after the initial 403 (initial + 1 retry), then give up")
}

// vault.New's startup probe (design §5's "Transit key missing" and
// "Permission denied" startup causes) must fire distinct, actionable errors
// instead of letting the controller start cleanly and fail on the first
// real secret write. These three tests cover the classification
// probeKeyExists/probeEncrypt now produce:
//
//   - a 404 reading the key -> key missing
//   - a 403 reading the key -> the policy lacks `read` on <mount>/keys/<key>
//   - a 403 encrypting (key read having already succeeded) -> the policy
//     lacks `update` on <mount>/encrypt/<key>
//
// Before this fix, the encrypt call alone had to guess "missing" vs "denied"
// from its own status code, and under the documented least-privilege policy
// (update only, no create) a missing key came back as 403 — indistinguishable
// from a policy that actually lacked `update` — because Vault ACL-checks an
// encrypt against a nonexistent key as a CreateOperation before Transit's own
// "key not found" handler ever runs.
func TestKeyManager_New_FailsWhenTransitKeyMissing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token/lookup-self", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": map[string]any{"ttl": 3600, "renewable": true}})
	})
	mux.HandleFunc("/v1/transit/keys/", func(w http.ResponseWriter, r *http.Request) {
		writeMissingKey(w)
	})
	mux.HandleFunc("/v1/transit/encrypt/", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("encrypt must not be attempted when the key read already reports the key missing")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, err := New(context.Background(), Config{
		Address: srv.URL, Mount: "transit", Key: "typo-key",
		Auth: "token", Token: "s.test",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "typo-key", "the error must name the key")
	assert.Contains(t, err.Error(), "vault write -f transit/keys/typo-key",
		"the error must give the exact command to create the missing key")
}

// A policy that omits `read` on <mount>/keys/<key> — the capability this fix
// adds to the documented policy — must be told exactly that, not misreported
// as a missing key or an opaque failure.
func TestKeyManager_New_FailsWhenKeyReadDenied(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token/lookup-self", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": map[string]any{"ttl": 3600, "renewable": true}})
	})
	mux.HandleFunc("/v1/transit/keys/", func(w http.ResponseWriter, r *http.Request) {
		writeKeyReadDenied(w)
	})
	mux.HandleFunc("/v1/transit/encrypt/", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("encrypt must not be attempted when the key read is itself denied")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, err := New(context.Background(), Config{
		Address: srv.URL, Mount: "transit", Key: "unified-cd-kek",
		Auth: "token", Token: "s.test",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transit/keys/unified-cd-kek", "the error must name the capability path needed")
	assert.Contains(t, err.Error(), "read", "the error must name the capability needed")
	assert.Contains(t, err.Error(), "cannot verify the key exists",
		"the error must explain the consequence, not just the missing grant")
}

func TestKeyManager_New_FailsWhenEncryptDenied(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token/lookup-self", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": map[string]any{"ttl": 3600, "renewable": true}})
	})
	mux.HandleFunc("/v1/transit/keys/", func(w http.ResponseWriter, r *http.Request) {
		writeExistingKey(w, r)
	})
	mux.HandleFunc("/v1/transit/encrypt/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{"permission denied"}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, err := New(context.Background(), Config{
		Address: srv.URL, Mount: "transit", Key: "unified-cd-kek",
		Auth: "token", Token: "s.test",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transit/encrypt/unified-cd-kek", "the error must name the capability path needed")
	assert.Contains(t, err.Error(), "update", "the error must name the capability needed")
}
