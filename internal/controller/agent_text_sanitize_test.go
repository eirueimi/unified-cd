package controller

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

// TestSanitizeAgentText_CleanInputUnchanged pins the fast path: a line with
// no NUL byte and valid UTF-8 comes back byte-for-byte identical and
// reports changed=false, so callers can skip any "sanitized" bookkeeping
// for the overwhelmingly common case.
func TestSanitizeAgentText_CleanInputUnchanged(t *testing.T) {
	for _, s := range []string{
		"",
		"plain ascii line",
		"unicode: héllo wörld 日本語 🎉",
		"line with the replacement character itself: � end",
	} {
		out, changed := sanitizeAgentText(s)
		assert.Equal(t, s, out)
		assert.False(t, changed, "input %q should not be reported as changed", s)
	}
}

// TestSanitizeAgentText_NULByte covers the exact defect this change fixes:
// PostgreSQL's text type rejects an embedded NUL (SQLSTATE 22021) even
// though NUL is technically valid UTF-8 (U+0000), so it needs its own check
// independent of utf8.ValidString.
func TestSanitizeAgentText_NULByte(t *testing.T) {
	out, changed := sanitizeAgentText("b\x00d")
	assert.True(t, changed)
	assert.Equal(t, "b�d", out)
	assert.True(t, utf8.ValidString(out), "sanitized output must always be valid UTF-8")
}

// TestSanitizeAgentText_MultipleNULs confirms every occurrence is replaced,
// not just the first.
func TestSanitizeAgentText_MultipleNULs(t *testing.T) {
	out, changed := sanitizeAgentText("\x00a\x00b\x00")
	assert.True(t, changed)
	assert.Equal(t, "�a�b�", out)
}

// TestSanitizeAgentText_InvalidUTF8 covers the second, independent failure
// mode: PostgreSQL also rejects byte sequences that are not valid UTF-8 at
// all, which is a different SQLSTATE trigger than the NUL case and must be
// handled by the same pass or the wedge just moves to a different byte.
func TestSanitizeAgentText_InvalidUTF8(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "lone continuation byte",
			in:   "a\x80b",
			want: "a�b",
		},
		{
			name: "truncated two-byte sequence",
			in:   "a\xc3b", // 0xC3 wants a continuation byte; 'b' is not one
			want: "a�b",
		},
		{
			name: "invalid start byte",
			in:   "a\xffb",
			want: "a�b",
		},
		{
			name: "overlong encoding of NUL (0xC0 0x80)",
			in:   "a\xc0\x80b",
			want: "a��b",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, changed := sanitizeAgentText(c.in)
			assert.True(t, changed)
			assert.Equal(t, c.want, out)
			assert.True(t, utf8.ValidString(out))
		})
	}
}

// TestSanitizeAgentText_GoodLinesAroundBadOneUnaffected confirms the
// surrounding good bytes in a line are preserved verbatim -- only the
// offending byte(s) are touched.
func TestSanitizeAgentText_GoodLinesAroundBadOneUnaffected(t *testing.T) {
	in := "before-" + strings.Repeat("x", 50) + "\x00" + strings.Repeat("y", 50) + "-after"
	out, changed := sanitizeAgentText(in)
	assert.True(t, changed)
	assert.True(t, strings.HasPrefix(out, "before-"+strings.Repeat("x", 50)))
	assert.True(t, strings.HasSuffix(out, strings.Repeat("y", 50)+"-after"))
	assert.Contains(t, out, "�")
	assert.NotContains(t, out, "\x00")
}
