package secrets

import (
	"encoding/base64"
	"net/url"
	"sort"
	"strings"
)

// minMaskLineLen is the minimum trimmed length for a line of a multi-line
// secret to become its own pattern. Shorter fragments (e.g. the "==" tail of
// a base64 block) would catastrophically over-mask unrelated output. This
// still leaves a residual trade at >= minMaskLineLen: a generic short line
// inside a multi-line secret (e.g. a bare "true" in a JSON-shaped secret)
// becomes a pattern too, so unrelated log text containing that same line can
// be over-masked, and an unrelated output value containing it can be dropped
// by Detects — a cost that is at least discoverable via the outputs skip
// warning rather than silent data loss.
const minMaskLineLen = 4

// Masker masks sensitive information in output.
type Masker struct {
	patterns []string
}

// NoOpMasker is a Masker that masks nothing.
var NoOpMasker = &Masker{}

// NewMasker creates a Masker from a list of sensitive values.
// For every value it registers exact-match, Base64-encoded, and URL-encoded
// patterns. Multi-line values additionally register each trimmed line
// (>= minMaskLineLen) as its own pattern, because masking is applied per log
// line and the whole value can never match a single line. Patterns are kept
// longest-first so a secret that contains another secret as a substring is
// replaced before the shorter one can split it.
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
		add(v)
		add(base64.StdEncoding.EncodeToString([]byte(v)))
		add(url.QueryEscape(v))
		if strings.Contains(v, "\n") {
			for _, ln := range strings.Split(v, "\n") {
				ln = strings.TrimSpace(ln)
				if len(ln) >= minMaskLineLen {
					add(ln)
				}
			}
		}
	}
	sort.SliceStable(patterns, func(i, j int) bool { return len(patterns[i]) > len(patterns[j]) })
	return &Masker{patterns: patterns}
}

// Mask replaces all registered patterns with "***".
func (m *Masker) Mask(line string) string {
	for _, p := range m.patterns {
		line = strings.ReplaceAll(line, p, "***")
	}
	return line
}

// Detects reports whether s contains any registered secret pattern.
func (m *Masker) Detects(s string) bool {
	for _, p := range m.patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
