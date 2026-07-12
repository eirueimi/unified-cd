package objectstore

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config holds configuration for an S3-compatible object store.
type S3Config struct {
	Endpoint        string // e.g. "s3.amazonaws.com" or "localhost:9000" for MinIO
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	UseSSL          bool
	Region          string
}

// S3ObjectStore implements ObjectStore using an S3-compatible backend (AWS S3, MinIO, etc.).
type S3ObjectStore struct {
	client *minio.Client
	bucket string
}

// NewS3ObjectStore creates an S3ObjectStore and ensures the bucket exists.
func NewS3ObjectStore(ctx context.Context, cfg S3Config) (*S3ObjectStore, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("minio client: %w", err)
	}

	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("bucket exists check: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("make bucket: %w", err)
		}
	}

	return &S3ObjectStore{client: client, bucket: cfg.Bucket}, nil
}

func (s *S3ObjectStore) Put(ctx context.Context, key string, content io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, content, size,
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return fmt.Errorf("put object %q: %w", key, err)
	}
	return nil
}

// Get implements ObjectStore.Get. See the ErrNotFound doc comment for the
// eager-detection contract this method must satisfy.
//
// minio-go's GetObject never errors for a missing key at call time — the
// underlying HTTP request is deferred until the first Read of the returned
// object. To honor the eager contract, Get forces that request now by
// peeking a single byte and translates a NotFound response into ErrNotFound
// before returning, instead of letting it surface later on Read. Peeking
// (rather than obj.Stat()) keeps the cost to ONE HTTP request per fetch:
// Stat issues a separate HEAD and the caller's first Read would still open
// its own GET. An io.EOF peek is a present-but-empty object, not an error.
func (s *S3ObjectStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get object %q: %w", key, err)
	}
	br := bufio.NewReader(obj)
	if _, err := br.Peek(1); err != nil && err != io.EOF {
		_ = obj.Close()
		errResp := minio.ToErrorResponse(err)
		if errResp.StatusCode == http.StatusNotFound || errResp.Code == "NoSuchKey" || errResp.Code == "NoSuchBucket" {
			return nil, fmt.Errorf("get object %q: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("get object %q: %w", key, err)
	}
	return &bufferedObject{r: br, c: obj}, nil
}

// bufferedObject keeps the peeked byte readable while delegating Close to
// the underlying minio object.
type bufferedObject struct {
	r *bufio.Reader
	c io.Closer
}

func (b *bufferedObject) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *bufferedObject) Close() error               { return b.c.Close() }

func (s *S3ObjectStore) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete object %q: %w", key, err)
	}
	return nil
}

func (s *S3ObjectStore) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var firstErr error
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix}) {
		if obj.Err != nil {
			firstErr = fmt.Errorf("list objects: %w", obj.Err)
			continue // drain channel to avoid goroutine leak
		}
		if firstErr == nil {
			keys = append(keys, obj.Key)
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return keys, nil
}
