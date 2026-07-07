package secrets

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMasker_MasksExactValue(t *testing.T) {
	m := NewMasker([]string{"s3cr3t"})
	assert.Equal(t, "before *** after", m.Mask("before s3cr3t after"))
}

func TestMasker_MasksBase64Variant(t *testing.T) {
	m := NewMasker([]string{"s3cr3t"})
	// base64("s3cr3t") = "czNjcjN0"
	assert.Equal(t, "prefix *** suffix", m.Mask("prefix czNjcjN0 suffix"))
}

func TestMasker_MultipleSecrets(t *testing.T) {
	m := NewMasker([]string{"alpha", "beta"})
	assert.Equal(t, "*** and ***", m.Mask("alpha and beta"))
}

func TestMasker_EmptyPatterns(t *testing.T) {
	m := NewMasker(nil)
	assert.Equal(t, "nothing to mask", m.Mask("nothing to mask"))
}

func TestMasker_NoMatch(t *testing.T) {
	m := NewMasker([]string{"xyz"})
	assert.Equal(t, "no match here", m.Mask("no match here"))
}

func TestMasker_MultilineSecretMasksPerLine(t *testing.T) {
	key := "-----BEGIN PRIVATE KEY-----\nMIIEvgIBADANBgkq\nhkiG9w0BAQEFAASC\n-----END PRIVATE KEY-----"
	m := NewMasker([]string{key})
	// each line of a multi-line secret is masked on its own
	assert.Equal(t, "***", m.Mask("MIIEvgIBADANBgkq"))
	assert.Equal(t, "leaked: ***", m.Mask("leaked: hkiG9w0BAQEFAASC"))
	assert.Equal(t, "***", m.Mask("-----BEGIN PRIVATE KEY-----"))
}

func TestMasker_MultilineSecretTrimsCR(t *testing.T) {
	m := NewMasker([]string{"lineone-value\r\nlinetwo-value"})
	assert.Equal(t, "got ***", m.Mask("got lineone-value"))
	assert.Equal(t, "got ***", m.Mask("got linetwo-value"))
}

func TestMasker_ShortLinesNotRegistered(t *testing.T) {
	// "==" and "zz" are shorter than minMaskLineLen after trimming and must
	// not become patterns (they would over-mask unrelated output).
	m := NewMasker([]string{"abcdefgh\n==\nzz"})
	assert.Equal(t, "x == y", m.Mask("x == y"))
	assert.Equal(t, "zz top", m.Mask("zz top"))
	assert.Equal(t, "*** !", m.Mask("abcdefgh !"))
}

func TestMasker_PrefixSecretPairMasksLongestFirst(t *testing.T) {
	// registration order must not matter: the longer secret wins first
	m := NewMasker([]string{"tok_abc", "tok_abcdef"})
	assert.Equal(t, "have ***", m.Mask("have tok_abcdef"))
	assert.Equal(t, "have ***", m.Mask("have tok_abc"))
}

func TestMasker_Detects(t *testing.T) {
	m := NewMasker([]string{"s3cr3t"})
	assert.True(t, m.Detects("prefix s3cr3t suffix"))
	// encoded variants count as detections too
	assert.True(t, m.Detects("czNjcjN0"))
	assert.False(t, m.Detects("clean value"))
	assert.False(t, NoOpMasker.Detects("s3cr3t"))
}
