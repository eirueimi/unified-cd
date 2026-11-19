package objectstore

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// newS3ForTest builds a store without contacting the endpoint. With a region
// set, presigning is a pure local computation.
func newS3ForTest(t *testing.T, endpoint, bucket, region string) *S3ObjectStore {
	t.Helper()
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4("AKIATEST", "secretsecretsecret", ""),
		Secure: true,
		Region: region,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &S3ObjectStore{client: client, bucket: bucket}
}

func TestPresign_EachURLIsOneObjectOneOp_15Minutes(t *testing.T) {
	s := newS3ForTest(t, "s3.example.com", "bkt", "us-east-1")
	ctx := context.Background()
	const key = "runs/1/out.tar.gz"

	cases := map[string]func() (*url.URL, error){
		"get":  func() (*url.URL, error) { return s.PresignGet(ctx, key, "") },
		"head": func() (*url.URL, error) { return s.PresignHead(ctx, key) },
		"put":  func() (*url.URL, error) { return s.PresignPut(ctx, key) },
		"part": func() (*url.URL, error) { return s.PresignUploadPart(ctx, key, "U", 3) },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			u, err := fn()
			if err != nil {
				t.Fatal(err)
			}
			if u.Path != "/bkt/"+key {
				t.Errorf("path = %q", u.Path)
			}
			q := u.Query()
			if q.Get("X-Amz-Expires") != "900" {
				t.Errorf("X-Amz-Expires = %q", q.Get("X-Amz-Expires"))
			}
			if q.Get("X-Amz-SignedHeaders") != "host" {
				t.Errorf("X-Amz-SignedHeaders = %q", q.Get("X-Amz-SignedHeaders"))
			}
			if name == "part" {
				if q.Get("partNumber") != "3" || q.Get("uploadId") != "U" {
					t.Errorf("part query = %v", q)
				}
			} else if q.Has("partNumber") {
				t.Errorf("unexpected partNumber in %v", q)
			}
		})
	}
	if PresignExpiry != 15*time.Minute {
		t.Errorf("PresignExpiry = %v", PresignExpiry)
	}
}

func TestPresignGet_SetsContentDisposition(t *testing.T) {
	s := newS3ForTest(t, "s3.example.com", "bkt", "us-east-1")
	u, err := s.PresignGet(context.Background(), "k", "out.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("response-content-disposition"); got != `attachment; filename="out.tar.gz"` {
		t.Errorf("response-content-disposition = %q", got)
	}
}

func TestMultipartPresignSupported(t *testing.T) {
	if MultipartPresignSupported("storage.googleapis.com") {
		t.Error("GCS must stay false until the GCS finding records a verified pass")
	}
	for _, ep := range []string{"s3.amazonaws.com", "garage:3900"} {
		if !MultipartPresignSupported(ep) {
			t.Errorf("%s should support multipart presign", ep)
		}
	}
}

func TestLocalStat_NotFoundAndModTime(t *testing.T) {
	dir := t.TempDir()
	l := NewLocalObjectStore(dir)
	ctx := context.Background()
	if _, err := l.Stat(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "b"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := l.Stat(ctx, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if info.Key != "a/b" || info.Size != 5 || info.LastModified.IsZero() {
		t.Errorf("info = %+v", info)
	}
}
