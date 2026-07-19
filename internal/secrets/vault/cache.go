package vault

import (
	"container/list"
	"sync"
	"time"
)

// dekCache caches unwrapped DEKs so a job claim carrying several secrets does
// not call Transit once per secret.
//
// This is load reduction and a cushion for brief outages — NOT a substitute for
// Vault HA. A job requesting an uncached secret while Vault is unreachable
// still fails.
//
// Relative to local-key mode, where the KEK sits in process memory for the
// controller's whole life, holding a few DEKs for a few minutes is a stricter
// posture, not a new class of exposure.
type dekCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List // front = most recently used
	cap     int
	ttl     time.Duration
	now     func() time.Time
}

type dekEntry struct {
	key       string
	dek       []byte
	expiresAt time.Time
}

func newDEKCache(capacity int, ttl time.Duration, now func() time.Time) *dekCache {
	if now == nil {
		now = time.Now
	}
	return &dekCache{
		entries: make(map[string]*list.Element),
		lru:     list.New(),
		cap:     capacity,
		ttl:     ttl,
		now:     now,
	}
}

// get returns a COPY of the cached DEK.
//
// A copy is mandatory, not defensive style: secrets.Decrypt zeroes the DEK
// returned by DecryptKey on every call via a defer
// (internal/secrets/crypto.go:66-72). Handing out the cache's own slice would
// let the caller zero the entry, and every later hit would return zeros.
func (c *dekCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	entry := el.Value.(*dekEntry)
	if !c.now().Before(entry.expiresAt) {
		c.removeLocked(el)
		return nil, false
	}
	c.lru.MoveToFront(el)
	out := make([]byte, len(entry.dek))
	copy(out, entry.dek)
	return out, true
}

// put stores a copy of dek, so a caller reusing or zeroing its buffer cannot
// corrupt the entry.
func (c *dekCache) put(key string, dek []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.entries[key]; ok {
		c.removeLocked(el)
	}
	stored := make([]byte, len(dek))
	copy(stored, dek)
	el := c.lru.PushFront(&dekEntry{key: key, dek: stored, expiresAt: c.now().Add(c.ttl)})
	c.entries[key] = el

	for c.lru.Len() > c.cap {
		c.removeLocked(c.lru.Back())
	}
}

func (c *dekCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// removeLocked drops an entry and zeroes its plaintext, matching the zeroing
// discipline in internal/secrets/crypto.go.
func (c *dekCache) removeLocked(el *list.Element) {
	entry := el.Value.(*dekEntry)
	for i := range entry.dek {
		entry.dek[i] = 0
	}
	c.lru.Remove(el)
	delete(c.entries, entry.key)
}

// peekStored exposes the stored backing array for tests that verify zeroing.
// It deliberately does not copy.
func (c *dekCache) peekStored(key string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil
	}
	return el.Value.(*dekEntry).dek
}
