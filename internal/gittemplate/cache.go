package gittemplate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"

	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// Cache caches fetched git templates in an ObjectStore.
// Only fixed refs (semver tags, SHAs) are cached; mutable refs (branches) are always fetched.
//
// Entries are partitioned by scope: the credential the content was fetched
// with (Credential.CacheScope; "" = fetched anonymously). A caller only reads
// entries of the scope it was handed, so content fetched with a credential is
// never served to a caller that credential's policy does not allow.
type Cache struct {
	store objectstore.ObjectStore
}

// NewCache creates a Cache backed by the given ObjectStore.
func NewCache(store objectstore.ObjectStore) *Cache {
	return &Cache{store: store}
}

// Get returns cached YAML for uri in scope, or (nil, false) on miss.
// Always returns false for mutable refs.
func (c *Cache) Get(ctx context.Context, uri URI, scope string) ([]byte, bool) {
	if !uri.IsFixed() {
		return nil, false
	}
	rc, err := c.store.Get(ctx, cacheKey(uri, scope))
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

// Put stores YAML for uri in scope. No-op for mutable refs.
func (c *Cache) Put(ctx context.Context, uri URI, scope string, data []byte) {
	if !uri.IsFixed() {
		return
	}
	_ = c.store.Put(ctx, cacheKey(uri, scope), bytes.NewReader(data), int64(len(data)))
}

// cacheKeyPrefix is the ObjectStore prefix of template cache entries. The v2
// prefix leaves legacy unscoped entries ("git_templates/<hash>", which may
// hold content fetched with a credential) unreachable.
const cacheKeyPrefix = "git_templates/v2/"

// cacheKey returns the ObjectStore key for uri in scope.
func cacheKey(uri URI, scope string) string {
	raw := fmt.Sprintf("%s\x00%s/%s/%s/%s/%s", scope, uri.Host, uri.Owner, uri.Repo, uri.Ref, uri.Path)
	h := sha256.Sum256([]byte(raw))
	return cacheKeyPrefix + base64.RawURLEncoding.EncodeToString(h[:])
}
