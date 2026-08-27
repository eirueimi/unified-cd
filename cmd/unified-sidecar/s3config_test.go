package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The broker takes precedence over both the file and static credential
// paths §5.5 already defined — see internal/objectstore/env.go's precedence
// comment for why static must stay last (every existing deployment) and why
// this file exists at all (a network fetch belongs in the sidecar binary's
// own wiring, not inside objectstore.S3ConfigFromEnv, which stays
// synchronous and local-only).
func TestS3ConfigFromEnv_BrokerWinsOverFileAndStatic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req api.StoreCredentialsRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "the-projected-token", req.Token)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(api.StoreCredentialsResponse{
			Endpoint: "broker-endpoint:9000", Bucket: "broker-bucket", AccessKey: "AKID-BROKER", SecretKey: "secret-broker",
		}))
	}))
	defer srv.Close()

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("the-projected-token"), 0o600))
	credFile := filepath.Join(dir, "creds")
	require.NoError(t, os.WriteFile(credFile, []byte("UNIFIED_S3_KEY=AKIA-FILE\nUNIFIED_S3_SECRET=secret-file\n"), 0o600))

	t.Setenv(envBrokerURL, srv.URL)
	t.Setenv(envBrokerTokenFile, tokenFile)
	t.Setenv("UNIFIED_S3_ENDPOINT", "static-endpoint:9000")
	t.Setenv("UNIFIED_S3_BUCKET", "static-bucket")
	t.Setenv("UNIFIED_S3_KEY", "AKIA-STATIC")
	t.Setenv("UNIFIED_S3_SECRET", "secret-static")
	t.Setenv("UNIFIED_S3_CREDENTIAL_FILE", credFile)

	cfg, err := s3ConfigFromEnv(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "broker-endpoint:9000", cfg.Endpoint, "the broker's endpoint must win, not the static UNIFIED_S3_ENDPOINT")
	assert.Equal(t, "broker-bucket", cfg.Bucket)
	require.NotNil(t, cfg.Creds)
	v, err := cfg.Creds.Get()
	require.NoError(t, err)
	assert.Equal(t, "AKID-BROKER", v.AccessKeyID, "the broker's credential must win over both the file and the static pair")
}

// Existing deployments (neither broker env var set) must see byte-identical
// behavior: this is what makes landing the broker alongside the existing
// paths, not instead of them, actually true.
func TestS3ConfigFromEnv_FallsThroughToStaticWhenBrokerUnset(t *testing.T) {
	t.Setenv("UNIFIED_S3_ENDPOINT", "static-endpoint:9000")
	t.Setenv("UNIFIED_S3_BUCKET", "static-bucket")
	t.Setenv("UNIFIED_S3_KEY", "AKIA-STATIC")
	t.Setenv("UNIFIED_S3_SECRET", "secret-static")

	cfg, err := s3ConfigFromEnv(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "static-endpoint:9000", cfg.Endpoint)
	assert.Nil(t, cfg.Creds, "no broker or credential file configured; Creds must stay nil so NewS3ObjectStore falls back to the static pair")
	assert.Equal(t, "AKIA-STATIC", cfg.AccessKeyID)
}

// Setting one broker env var without the other is a configuration mistake,
// not a signal to silently fall back to file/static — that would serve the
// wrong credential shape with no error at all.
func TestS3ConfigFromEnv_RejectsPartialBrokerConfig(t *testing.T) {
	t.Setenv(envBrokerURL, "https://controller.example")
	t.Setenv("UNIFIED_S3_ENDPOINT", "static-endpoint:9000")
	t.Setenv("UNIFIED_S3_BUCKET", "static-bucket")
	t.Setenv("UNIFIED_S3_KEY", "AKIA-STATIC")
	t.Setenv("UNIFIED_S3_SECRET", "secret-static")

	_, err := s3ConfigFromEnv(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), envBrokerTokenFile)
}

// The failure message must name the controller and the reason — the
// original trap ("artifact requires S3 configuration (UNIFIED_S3_*)") named
// neither.
func TestS3ConfigFromEnv_BrokerFailureNamesControllerAndReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "store credentials rejected", http.StatusForbidden)
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("tok"), 0o600))
	t.Setenv(envBrokerURL, srv.URL)
	t.Setenv(envBrokerTokenFile, tokenFile)

	_, err := s3ConfigFromEnv(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), srv.URL, "must name the controller")
	assert.Contains(t, err.Error(), "403", "must name the reason")
}
