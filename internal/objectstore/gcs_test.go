//go:build gcs

package objectstore_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// TestGCS_PresignedMultipartTwoParts checks whether GCS's S3-compatible XML API
// accepts presigned UploadPart URLs, which decides MultipartPresignSupported's
// GCS branch. Needs HMAC credentials; skips without them.
func TestGCS_PresignedMultipartTwoParts(t *testing.T) {
	bucket := os.Getenv("UNIFIED_TEST_GCS_BUCKET")
	key := os.Getenv("UNIFIED_TEST_GCS_KEY")
	secret := os.Getenv("UNIFIED_TEST_GCS_SECRET")
	if bucket == "" || key == "" || secret == "" {
		t.Skip("UNIFIED_TEST_GCS_BUCKET/KEY/SECRET not set")
	}
	ctx := context.Background()
	s, err := objectstore.NewS3ObjectStore(ctx, objectstore.S3Config{
		Endpoint: "storage.googleapis.com", Bucket: bucket,
		AccessKeyID: key, SecretAccessKey: secret, UseSSL: true, Region: "auto",
	})
	if err != nil {
		t.Fatal(err)
	}
	obj := fmt.Sprintf("gcs-test/%d/multipart", time.Now().UnixNano())
	t.Cleanup(func() { _ = s.Delete(ctx, obj) })

	id, err := s.StartMultipart(ctx, obj)
	if err != nil {
		t.Fatal(err)
	}
	const mib5 = 5 << 20
	bodies := [][]byte{bytes.Repeat([]byte("a"), mib5), []byte("b")}
	var parts []objectstore.CompletedPart
	for i, body := range bodies {
		u, err := s.PresignUploadPart(ctx, obj, id, i+1)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPut, u.String(), bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			_ = s.AbortMultipart(ctx, obj, id)
			t.Fatalf("part %d status %d: %s", i+1, resp.StatusCode, b)
		}
		parts = append(parts, objectstore.CompletedPart{PartNumber: i + 1, ETag: strings.Trim(resp.Header.Get("ETag"), "\"")})
	}
	if err := s.CompleteMultipart(ctx, obj, id, parts); err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(ctx, obj)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != mib5+1 {
		t.Fatalf("size = %d, want %d", info.Size, mib5+1)
	}
}
