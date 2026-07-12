package objectstore

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is the sentinel error returned (wrapped) by Get when key does
// not exist in the store.
//
// Contract for implementers: missing-key detection MUST be eager — Get must
// determine whether key exists before returning, not lazily on the first
// Read of the returned io.ReadCloser. This matters because minio-go's
// GetObject (used by S3ObjectStore) returns a reader immediately without
// checking for existence; the "key not found" error only surfaces on first
// Read, deep inside whatever is consuming the stream (e.g. a tar/zstd
// decoder), which is too late for a caller to distinguish "nothing to
// restore" from "the archive is corrupt". Every implementation (S3, local
// filesystem, or any test fake) must perform the existence check inside Get
// and return an error satisfying errors.Is(err, ErrNotFound) — never leave
// it to be discovered lazily by the caller.
var ErrNotFound = errors.New("objectstore: object not found")

// ObjectStore abstracts binary object storage (local filesystem or S3-compatible).
type ObjectStore interface {
	// Put stores content under key. size is the byte count of content.
	Put(ctx context.Context, key string, content io.Reader, size int64) error
	// Get retrieves the object at key. Caller must Close the returned reader.
	// If key does not exist, Get returns an error satisfying
	// errors.Is(err, ErrNotFound); this check is eager (performed before Get
	// returns), never deferred to the first Read of the result. See
	// ErrNotFound for the full contract.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the object. Returns nil if not found.
	Delete(ctx context.Context, key string) error
	// List returns all object keys that start with prefix. An empty prefix returns all keys.
	List(ctx context.Context, prefix string) ([]string, error)
}
