package objectstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestS3ConfigFromEnv_OK(t *testing.T) {
	t.Setenv("UNIFIED_S3_ENDPOINT", "s3:9000")
	t.Setenv("UNIFIED_S3_BUCKET", "b")
	t.Setenv("UNIFIED_S3_KEY", "k")
	t.Setenv("UNIFIED_S3_SECRET", "s")
	t.Setenv("UNIFIED_S3_USE_SSL", "true")
	t.Setenv("UNIFIED_S3_REGION", "us-east-1")
	cfg, err := S3ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "s3:9000" || cfg.Bucket != "b" || cfg.AccessKeyID != "k" || cfg.SecretAccessKey != "s" || !cfg.UseSSL || cfg.Region != "us-east-1" {
		t.Fatalf("got %+v", cfg)
	}
}

func TestS3ConfigFromEnv_MissingRequired(t *testing.T) {
	t.Setenv("UNIFIED_S3_ENDPOINT", "s3:9000")
	t.Setenv("UNIFIED_S3_BUCKET", "")
	t.Setenv("UNIFIED_S3_KEY", "k")
	t.Setenv("UNIFIED_S3_SECRET", "s")
	if _, err := S3ConfigFromEnv(); err == nil {
		t.Fatal("expected error for missing bucket")
	}
}

// The file wins when both are set: an operator who mounted a credential file
// meant to use it, and silently preferring the stale env pair would be the
// hardest possible thing to debug.
func TestS3ConfigFromEnv_FileWinsOverStatic(t *testing.T) {
	dir := t.TempDir()
	p := writeCredFile(t, dir, "AKIA-FILE", "secret-file")
	t.Setenv("UNIFIED_S3_ENDPOINT", "s3:9000")
	t.Setenv("UNIFIED_S3_BUCKET", "b")
	t.Setenv("UNIFIED_S3_KEY", "AKIA-STATIC")
	t.Setenv("UNIFIED_S3_SECRET", "secret-static")
	t.Setenv("UNIFIED_S3_CREDENTIAL_FILE", p)

	cfg, err := S3ConfigFromEnv()
	require.NoError(t, err)
	require.NotNil(t, cfg.Creds, "a credential file was set; cfg.Creds must be populated instead of falling through to the static pair")

	v, err := cfg.Creds.Get()
	require.NoError(t, err)
	assert.Equal(t, "AKIA-FILE", v.AccessKeyID,
		"the file must win over UNIFIED_S3_KEY/UNIFIED_S3_SECRET when both are configured")
}

// Existing deployments set only the key pair. This is the test that says the
// change is safe to ship.
func TestS3ConfigFromEnv_StaticStillWorksAlone(t *testing.T) {
	t.Setenv("UNIFIED_S3_ENDPOINT", "s3:9000")
	t.Setenv("UNIFIED_S3_BUCKET", "b")
	t.Setenv("UNIFIED_S3_KEY", "k")
	t.Setenv("UNIFIED_S3_SECRET", "s")

	cfg, err := S3ConfigFromEnv()
	require.NoError(t, err)
	assert.Nil(t, cfg.Creds, "no credential file configured; Creds must stay nil so NewS3ObjectStore falls back to static")
	assert.Equal(t, "k", cfg.AccessKeyID)
	assert.Equal(t, "s", cfg.SecretAccessKey)
}

// Neither configured: the error must name both routes.
func TestS3ConfigFromEnv_ErrorNamesBothRoutes(t *testing.T) {
	t.Setenv("UNIFIED_S3_ENDPOINT", "s3:9000")
	t.Setenv("UNIFIED_S3_BUCKET", "b")
	t.Setenv("UNIFIED_S3_KEY", "")
	t.Setenv("UNIFIED_S3_SECRET", "")
	t.Setenv("UNIFIED_S3_CREDENTIAL_FILE", "")

	_, err := S3ConfigFromEnv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNIFIED_S3_KEY")
	assert.Contains(t, err.Error(), "UNIFIED_S3_SECRET")
	assert.Contains(t, err.Error(), "UNIFIED_S3_CREDENTIAL_FILE")
}
