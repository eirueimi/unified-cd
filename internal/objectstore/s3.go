package objectstore

import (
	"context"
	"fmt"
	"io"

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

func (s *S3ObjectStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get object %q: %w", key, err)
	}
	return obj, nil
}

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
