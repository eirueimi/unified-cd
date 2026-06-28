package objectstore

import (
	"context"
	"io"
)

// ObjectStore abstracts binary object storage (local filesystem or S3-compatible).
type ObjectStore interface {
	// Put stores content under key. size is the byte count of content.
	Put(ctx context.Context, key string, content io.Reader, size int64) error
	// Get retrieves the object at key. Caller must Close the returned reader.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the object. Returns nil if not found.
	Delete(ctx context.Context, key string) error
	// List returns all object keys that start with prefix. An empty prefix returns all keys.
	List(ctx context.Context, prefix string) ([]string, error)
}
