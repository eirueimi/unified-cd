package objectstore

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
)

var _ Presigner = (*S3ObjectStore)(nil)

// MultipartPresignSupported reports whether the backend at endpoint accepts
// presigned UploadPart URLs. GCS's S3-compatible XML API has not been verified
// for this (see "Object store compatibility" in the operations guide), so it
// stays false and clients fall back to a single presigned PUT.
func MultipartPresignSupported(endpoint string) bool {
	host := strings.ToLower(endpoint)
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	return host != "storage.googleapis.com"
}

// MultipartPresign implements Presigner.
func (s *S3ObjectStore) MultipartPresign() bool { return s.multipartPresign }

func (s *S3ObjectStore) presign(ctx context.Context, method, key string, params url.Values) (*url.URL, error) {
	u, err := s.client.Presign(ctx, method, s.bucket, key, PresignExpiry, params)
	if err != nil {
		return nil, fmt.Errorf("presign %s %q: %w", method, key, err)
	}
	return u, nil
}

// PresignGet implements Presigner.
func (s *S3ObjectStore) PresignGet(ctx context.Context, key, filename string) (*url.URL, error) {
	var params url.Values
	if filename != "" {
		params = url.Values{"response-content-disposition": {fmt.Sprintf("attachment; filename=%q", filename)}}
	}
	return s.presign(ctx, http.MethodGet, key, params)
}

// PresignHead implements Presigner.
func (s *S3ObjectStore) PresignHead(ctx context.Context, key string) (*url.URL, error) {
	return s.presign(ctx, http.MethodHead, key, nil)
}

// PresignPut implements Presigner.
func (s *S3ObjectStore) PresignPut(ctx context.Context, key string) (*url.URL, error) {
	return s.presign(ctx, http.MethodPut, key, nil)
}

// PresignUploadPart implements Presigner.
func (s *S3ObjectStore) PresignUploadPart(ctx context.Context, key, uploadID string, part int) (*url.URL, error) {
	return s.presign(ctx, http.MethodPut, key, url.Values{
		"partNumber": {fmt.Sprint(part)},
		"uploadId":   {uploadID},
	})
}

func (s *S3ObjectStore) core() minio.Core { return minio.Core{Client: s.client} }

// StartMultipart implements Presigner.
func (s *S3ObjectStore) StartMultipart(ctx context.Context, key string) (string, error) {
	id, err := s.core().NewMultipartUpload(ctx, s.bucket, key, minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return "", fmt.Errorf("start multipart %q: %w", key, err)
	}
	return id, nil
}

// CompleteMultipart implements Presigner.
func (s *S3ObjectStore) CompleteMultipart(ctx context.Context, key, uploadID string, parts []CompletedPart) error {
	cp := make([]minio.CompletePart, len(parts))
	for i, p := range parts {
		cp[i] = minio.CompletePart{PartNumber: p.PartNumber, ETag: p.ETag}
	}
	if _, err := s.core().CompleteMultipartUpload(ctx, s.bucket, key, uploadID, cp, minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("complete multipart %q: %w", key, err)
	}
	return nil
}

// AbortMultipart implements Presigner.
func (s *S3ObjectStore) AbortMultipart(ctx context.Context, key, uploadID string) error {
	if err := s.core().AbortMultipartUpload(ctx, s.bucket, key, uploadID); err != nil {
		return fmt.Errorf("abort multipart %q: %w", key, err)
	}
	return nil
}

// ListMultipartUploads implements Presigner, paging through all results.
func (s *S3ObjectStore) ListMultipartUploads(ctx context.Context, prefix string) ([]Upload, error) {
	var out []Upload
	keyMarker, idMarker := "", ""
	for {
		res, err := s.core().ListMultipartUploads(ctx, s.bucket, prefix, keyMarker, idMarker, "", 1000)
		if err != nil {
			return nil, fmt.Errorf("list multipart uploads: %w", err)
		}
		for _, u := range res.Uploads {
			out = append(out, Upload{Key: u.Key, UploadID: u.UploadID, Initiated: u.Initiated})
		}
		if !res.IsTruncated {
			return out, nil
		}
		keyMarker, idMarker = res.NextKeyMarker, res.NextUploadIDMarker
	}
}

// Stat implements Statter, mapping a missing key to ErrNotFound.
func (s *S3ObjectStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	oi, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		er := minio.ToErrorResponse(err)
		if er.StatusCode == http.StatusNotFound || er.Code == "NoSuchKey" {
			return ObjectInfo{}, fmt.Errorf("stat %q: %w", key, ErrNotFound)
		}
		return ObjectInfo{}, fmt.Errorf("stat %q: %w", key, err)
	}
	return ObjectInfo{Key: key, Size: oi.Size, ETag: oi.ETag, LastModified: oi.LastModified}, nil
}
