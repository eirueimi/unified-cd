package secrets

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The whole point of the AAD is that a ciphertext cannot be moved between
// rows. If two different identities can produce the same canonical bytes,
// that protection silently disappears — so ambiguity is tested directly.
func TestBinding_SecretNamesDoNotCollide(t *testing.T) {
	// The length prefix guarantees two different names encode differently.
	assert.NotEqual(t, SecretBinding("ab").canonical(), SecretBinding("a").canonical())
	assert.NotEqual(t, SecretBinding("a").canonical(), SecretBinding("").canonical())
}

func TestBinding_CanonicalIsStable(t *testing.T) {
	assert.Equal(t, SecretBinding("NAME").canonical(), SecretBinding("NAME").canonical())
}

// A secret and a session-refresh binding with the same field must differ.
func TestBinding_KindsDoNotCollide(t *testing.T) {
	assert.NotEqual(t, SecretBinding("x").canonical(), SessionRefreshBinding("x").canonical())
}

func TestBinding_StringDescribesIdentity(t *testing.T) {
	b := SecretBinding("AWS_KEY")
	require.Contains(t, b.String(), "secret")
	assert.Contains(t, b.String(), "AWS_KEY")
}
