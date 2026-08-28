package objectstore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTokenFile(t *testing.T, dir, token string) string {
	t.Helper()
	p := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(p, []byte(token), 0o600))
	return p
}

// brokerStub serves one canned api.StoreCredentialsResponse per call and
// records the last token it was presented, so tests can assert what the
// provider sent without a real controller.
type brokerStub struct {
	response  api.StoreCredentialsResponse
	status    int
	calls     int
	lastToken string
	lastRunID string
}

func (b *brokerStub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b.calls++
		var req api.StoreCredentialsRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		b.lastToken = req.Token
		b.lastRunID = req.RunID
		if b.status != 0 && b.status != http.StatusOK {
			http.Error(w, "rejected", b.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(b.response))
	}
}

func TestBrokerConfig_FetchesEndpointBucketAndCredential(t *testing.T) {
	stub := &brokerStub{response: api.StoreCredentialsResponse{
		Endpoint: "s3.example.internal:9000", Bucket: "artifacts", Region: "us-east-1", UseSSL: true,
		AccessKey: "AKID1", SecretKey: "secret1",
	}}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	tokenFile := writeTokenFile(t, t.TempDir(), "the-projected-token")

	cfg, err := BrokerConfig(t.Context(), srv.URL, tokenFile, "")
	require.NoError(t, err)
	assert.Equal(t, "s3.example.internal:9000", cfg.Endpoint)
	assert.Equal(t, "artifacts", cfg.Bucket)
	assert.Equal(t, "us-east-1", cfg.Region)
	assert.True(t, cfg.UseSSL)
	require.NotNil(t, cfg.Creds, "BrokerConfig must populate Creds instead of leaving NewS3ObjectStore to fall back to static")

	v, err := cfg.Creds.Get()
	require.NoError(t, err)
	assert.Equal(t, "AKID1", v.AccessKeyID)
	assert.Equal(t, "secret1", v.SecretAccessKey)
	assert.Equal(t, "the-projected-token", stub.lastToken, "the provider must present the token read from tokenFile")
	assert.Equal(t, 1, stub.calls, "the initial BrokerConfig fetch must not be repeated by the first Get()")
}

// TestBrokerConfig_ForwardsRunID pins the fix for PR #159's known gap
// (RunID always sent empty): BrokerConfig's runID parameter must reach the
// controller on both the initial fetch AND a later refetch after expiry —
// it does not change across the sidecar process's lifetime the way the
// token does, so every fetch must carry it, not just the first.
func TestBrokerConfig_ForwardsRunID(t *testing.T) {
	stub := &brokerStub{response: api.StoreCredentialsResponse{
		Endpoint: "s3:9000", Bucket: "b", AccessKey: "AKID1", SecretKey: "secret1",
		ExpiresAt: time.Now().Add(-time.Second), // forces a refetch on the next Get()
	}}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	tokenFile := writeTokenFile(t, t.TempDir(), "tok")

	cfg, err := BrokerConfig(t.Context(), srv.URL, tokenFile, "run-abc")
	require.NoError(t, err)
	assert.Equal(t, "run-abc", stub.lastRunID, "the initial fetch must carry runID")

	stub.response.ExpiresAt = time.Now().Add(-time.Second)
	_, err = cfg.Creds.Get()
	require.NoError(t, err)
	assert.Equal(t, "run-abc", stub.lastRunID, "a refetch after expiry must still carry runID")
	assert.Equal(t, 2, stub.calls)
}

func TestBrokerConfig_ErrorsWhenTheControllerRejects(t *testing.T) {
	stub := &brokerStub{status: http.StatusForbidden}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	tokenFile := writeTokenFile(t, t.TempDir(), "bad-token")

	_, err := BrokerConfig(t.Context(), srv.URL, tokenFile, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), srv.URL, "the error must name the controller, not just say credentials failed")
}

func TestBrokerConfig_ErrorsWhenTheTokenFileIsMissing(t *testing.T) {
	_, err := BrokerConfig(t.Context(), "http://controller.example", filepath.Join(t.TempDir(), "does-not-exist"), "")
	require.Error(t, err)
}

// A zero ExpiresAt (today's passthrough credential) must not be refetched on
// every Get() — that would put every artifact/cache operation on the
// controller's hot path for no benefit, since the credential never changes
// on its own.
func TestBrokerCredentials_ZeroExpiryDoesNotRefetch(t *testing.T) {
	stub := &brokerStub{response: api.StoreCredentialsResponse{
		Endpoint: "s3:9000", Bucket: "b", AccessKey: "AKID1", SecretKey: "secret1",
	}}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	tokenFile := writeTokenFile(t, t.TempDir(), "tok")

	cfg, err := BrokerConfig(t.Context(), srv.URL, tokenFile, "")
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		_, err := cfg.Creds.Get()
		require.NoError(t, err)
	}
	assert.Equal(t, 1, stub.calls, "a never-expiring credential must be fetched once and reused")
}

// A credential carrying a real ExpiresAt DOES refresh — this is the whole
// point of the provider seam existing (§5.5): a future scoped/short-lived
// credential is a change of what the controller returns, and the sidecar's
// refresh logic must already work without being touched.
func TestBrokerCredentials_RefreshesAfterExpiry(t *testing.T) {
	stub := &brokerStub{response: api.StoreCredentialsResponse{
		Endpoint: "s3:9000", Bucket: "b", AccessKey: "AKID-OLD", SecretKey: "secret-old",
		ExpiresAt: time.Now().Add(-time.Second), // already expired at fetch time
	}}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	tokenFile := writeTokenFile(t, t.TempDir(), "tok")

	cfg, err := BrokerConfig(t.Context(), srv.URL, tokenFile, "")
	require.NoError(t, err)

	stub.response.AccessKey = "AKID-NEW"
	stub.response.SecretKey = "secret-new"
	stub.response.ExpiresAt = time.Now().Add(-time.Second)

	v, err := cfg.Creds.Get()
	require.NoError(t, err)
	assert.Equal(t, "AKID-NEW", v.AccessKeyID, "an expired credential must be refetched, picking up the rotated value")
	assert.Equal(t, 2, stub.calls)
}

// The token file is re-read on every fetch, not cached in memory: the
// kubelet rewrites it in place, and a stale in-memory copy would eventually
// be a token the API server has already rotated past.
func TestBrokerCredentials_RereadsTheTokenFileOnEachFetch(t *testing.T) {
	stub := &brokerStub{response: api.StoreCredentialsResponse{
		Endpoint: "s3:9000", Bucket: "b", AccessKey: "AKID1", SecretKey: "secret1",
		ExpiresAt: time.Now().Add(-time.Second),
	}}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	dir := t.TempDir()
	tokenFile := writeTokenFile(t, dir, "token-v1")

	cfg, err := BrokerConfig(t.Context(), srv.URL, tokenFile, "")
	require.NoError(t, err)
	assert.Equal(t, "token-v1", stub.lastToken)

	require.NoError(t, os.WriteFile(tokenFile, []byte("token-v2"), 0o600))
	stub.response.ExpiresAt = time.Now().Add(-time.Second)
	_, err = cfg.Creds.Get()
	require.NoError(t, err)
	assert.Equal(t, "token-v2", stub.lastToken, "the kubelet-rotated token must reach the next fetch")
}
