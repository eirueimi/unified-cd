package gittemplate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"

	"github.com/unified-cd/unified-cd/internal/objectstore"
)

// Cache caches fetched git templates in an ObjectStore.
// Only fixed refs (semver tags, SHAs) are cached; mutable refs (branches) are always fetched.
type Cache struct {
	store objectstore.ObjectStore
}

// NewCache creates a Cache backed by the given ObjectStore.
func NewCache(store objectstore.ObjectStore) *Cache {
	return &Cache{store: store}
}

// Get returns cached YAML for uri, or (nil, false) on miss.
// Always returns false for mutable refs.
func (c *Cache) Get(ctx context.Context, uri URI) ([]byte, bool) {
	if !uri.IsFixed() {
		return nil, false
	}
	key := cacheKey(uri)
	rc, err := c.store.Get(ctx, key)
	if err != nil {
		return nil, false
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, false
	}
	return data, true
}

// Put stores YAML for uri. No-op for mutable refs.
func (c *Cache) Put(ctx context.Context, uri URI, data []byte) {
	if !uri.IsFixed() {
		return
	}
	key := cacheKey(uri)
	_ = c.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)))
}

// cacheKey returns the ObjectStore key for the given URI.
func cacheKey(uri URI) string {
	raw := fmt.Sprintf("%s/%s/%s/%s/%s", uri.Host, uri.Owner, uri.Repo, uri.Ref, uri.Path)
	h := sha256.Sum256([]byte(raw))
	return "git_templates/" + base64.RawURLEncoding.EncodeToString(h[:])
}
