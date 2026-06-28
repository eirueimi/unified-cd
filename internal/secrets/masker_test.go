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
