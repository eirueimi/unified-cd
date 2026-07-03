package objectstore

import "testing"

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
