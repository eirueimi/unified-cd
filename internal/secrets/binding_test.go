package secrets

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The whole point of the AAD is that a ciphertext cannot be moved between
// rows. If two different identities can produce the same canonical bytes,
// that protection silently disappears — so ambiguity is tested directly.
func TestBinding_CanonicalIsUnambiguous(t *testing.T) {
	a := SecretBinding("a", "bc", "")
	b := SecretBinding("ab", "c", "")
	assert.NotEqual(t, a.canonical(), b.canonical(),
		`("a","bc") and ("ab","c") must not collide — length prefixes are required`)
}

func TestBinding_CanonicalIsStable(t *testing.T) {
	a := SecretBinding("NAME", "global", "")
	b := SecretBinding("NAME", "global", "")
	assert.Equal(t, a.canonical(), b.canonical())
}

func TestBinding_KindsDoNotCollide(t *testing.T) {
	s := SecretBinding("x", "", "")
	r := SessionRefreshBinding("x")
	assert.NotEqual(t, s.canonical(), r.canonical(),
		"a secret and a session-refresh binding with the same field must differ")
}

func TestBinding_EmptyFieldsAreDistinguished(t *testing.T) {
	a := SecretBinding("", "x", "")
	b := SecretBinding("x", "", "")
	assert.NotEqual(t, a.canonical(), b.canonical())
}

func TestBinding_StringDescribesIdentity(t *testing.T) {
	b := SecretBinding("AWS_KEY", "global", "")
	require.Contains(t, b.String(), "secret")
	assert.Contains(t, b.String(), "AWS_KEY")
}
