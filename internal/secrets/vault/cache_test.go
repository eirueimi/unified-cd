package vault

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDEKCache_HitAndMiss(t *testing.T) {
	clock := newTestClock()
	c := newDEKCache(4, time.Minute, clock.now)

	_, ok := c.get("a")
	assert.False(t, ok)

	c.put("a", []byte("dek-a"))
	got, ok := c.get("a")
	require.True(t, ok)
	assert.Equal(t, []byte("dek-a"), got)
}

// secrets.Decrypt zeroes the DEK returned by DecryptKey on every call, via a
// defer. If the cache handed out its own slice, the caller would zero the cache
// entry and every later hit would return zeros.
func TestDEKCache_ReturnsACopy(t *testing.T) {
	clock := newTestClock()
	c := newDEKCache(4, time.Minute, clock.now)
	c.put("a", []byte("dek-a"))

	first, ok := c.get("a")
	require.True(t, ok)
	for i := range first {
		first[i] = 0 // simulate the caller's zeroing defer
	}

	second, ok := c.get("a")
	require.True(t, ok)
	assert.Equal(t, []byte("dek-a"), second,
		"a caller zeroing its copy must not poison the cache")
}

// put must copy too, or the caller's buffer and the cache alias.
func TestDEKCache_CopiesOnPut(t *testing.T) {
	clock := newTestClock()
	c := newDEKCache(4, time.Minute, clock.now)

	dek := []byte("dek-a")
	c.put("a", dek)
	for i := range dek {
		dek[i] = 0
	}

	got, ok := c.get("a")
	require.True(t, ok)
	assert.Equal(t, []byte("dek-a"), got)
}

// The TTL is what makes a Transit key rotation or revocation take effect within
// a bounded window instead of never.
func TestDEKCache_ExpiresAfterTTL(t *testing.T) {
	clock := newTestClock()
	c := newDEKCache(4, time.Minute, clock.now)
	c.put("a", []byte("dek-a"))

	clock.advance(59 * time.Second)
	_, ok := c.get("a")
	assert.True(t, ok)

	clock.advance(2 * time.Second)
	_, ok = c.get("a")
	assert.False(t, ok, "an entry past its TTL must not be served")
	assert.Equal(t, 0, c.len(), "an entry past its TTL must be removed, not just skipped")
}

func TestDEKCache_EvictsLeastRecentlyUsed(t *testing.T) {
	clock := newTestClock()
	c := newDEKCache(2, time.Minute, clock.now)

	c.put("a", []byte("dek-a"))
	c.put("b", []byte("dek-b"))
	_, _ = c.get("a") // a is now more recently used than b
	c.put("c", []byte("dek-c"))

	assert.Equal(t, 2, c.len())
	_, ok := c.get("b")
	assert.False(t, ok, "the least recently used entry is evicted")
	_, ok = c.get("a")
	assert.True(t, ok)
}

// An evicted plaintext DEK must not linger in memory, matching the zeroing
// discipline in internal/secrets/crypto.go.
func TestDEKCache_ZeroesEvictedEntries(t *testing.T) {
	clock := newTestClock()
	c := newDEKCache(1, time.Minute, clock.now)

	c.put("a", []byte("dek-a"))
	evicted := c.peekStored("a")
	require.NotNil(t, evicted)

	c.put("b", []byte("dek-b"))

	assert.Equal(t, make([]byte, len(evicted)), evicted,
		"the evicted entry's backing array must be zeroed")
}
