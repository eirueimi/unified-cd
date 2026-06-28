package secrets

import (
	"encoding/base64"
	"net/url"
	"strings"
)

// Masker masks sensitive information in output.
type Masker struct {
	patterns []string
}

// NoOpMasker is a Masker that masks nothing.
var NoOpMasker = &Masker{}

// NewMasker creates a Masker from a list of sensitive values.
// It registers three patterns: exact match, Base64-encoded, and URL-encoded.
func NewMasker(values []string) *Masker {
	seen := map[string]struct{}{}
	var patterns []string
	add := func(s string) {
		if s == "" {
			return
		}
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			patterns = append(patterns, s)
		}
	}
	for _, v := range values {
		if v == "" {
			continue
		}
		// add exact-match pattern
		add(v)
		// add Base64-encoded version
		add(base64.StdEncoding.EncodeToString([]byte(v)))
		// add URL-encoded version
		add(url.QueryEscape(v))
	}
	return &Masker{patterns: patterns}
}

// Mask replaces all registered patterns with "***".
func (m *Masker) Mask(line string) string {
	for _, p := range m.patterns {
		line = strings.ReplaceAll(line, p, "***")
	}
	return line
}
