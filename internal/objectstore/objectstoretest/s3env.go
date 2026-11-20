// Package objectstoretest holds helpers for tests that run against a real
// S3-compatible store.
package objectstoretest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// S3FromEnv returns a store built from UNIFIED_TEST_S3_ENDPOINT, _BUCKET, _KEY
// and _SECRET (plus optional _REGION, default "garage", and _USE_SSL), skipping
// the test when any required variable is unset. The CI garage job sets them.
func S3FromEnv(t *testing.T) *objectstore.S3ObjectStore {
	t.Helper()
	endpoint := os.Getenv("UNIFIED_TEST_S3_ENDPOINT")
	bucket := os.Getenv("UNIFIED_TEST_S3_BUCKET")
	key := os.Getenv("UNIFIED_TEST_S3_KEY")
	secret := os.Getenv("UNIFIED_TEST_S3_SECRET")
	if endpoint == "" || bucket == "" || key == "" || secret == "" {
		t.Skip("UNIFIED_TEST_S3_ENDPOINT/BUCKET/KEY/SECRET not set")
	}
	region := os.Getenv("UNIFIED_TEST_S3_REGION")
	if region == "" {
		region = "garage"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := objectstore.NewS3ObjectStore(ctx, objectstore.S3Config{
		Endpoint:         endpoint,
		Bucket:           bucket,
		AccessKeyID:      key,
		SecretAccessKey:  secret,
		UseSSL:           os.Getenv("UNIFIED_TEST_S3_USE_SSL") == "true",
		Region:           region,
		MultipartPresign: objectstore.MultipartPresignSupported(endpoint),
	})
	if err != nil {
		t.Fatalf("connect to test S3 store: %v", err)
	}
	return s
}
