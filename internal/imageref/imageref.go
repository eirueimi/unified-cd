// Package imageref performs a conservative sanity check on container image
// references before they reach a container CLI's argv.
package imageref

import (
	"fmt"
	"strings"
	"unicode"
)

// Validate rejects image references that a docker-compatible CLI could
// misparse. It is deliberately permissive otherwise: registry/host:port/path,
// tags and @sha256 digests all pass; full reference grammar is left to the
// runtime.
//
// Rejected: empty, a leading '-' (parsed as a CLI option), whitespace and
// control characters.
func Validate(ref string) error {
	if ref == "" {
		return fmt.Errorf("image reference is empty")
	}
	if ref[0] == '-' {
		return fmt.Errorf("image reference %q must not start with '-'", ref)
	}
	for _, r := range ref {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("image reference %q must not contain whitespace or control characters", ref)
		}
	}
	return nil
}

// AlwaysPull reports whether ref names a mutable image that must be re-pulled
// before use: it has no digest and its tag is empty or "latest". The tag is
// the part after the last ':' that follows the last '/'.
func AlwaysPull(ref string) bool {
	if strings.Contains(ref, "@") {
		return false
	}
	name := ref
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	i := strings.LastIndex(name, ":")
	if i < 0 {
		return true
	}
	tag := name[i+1:]
	return tag == "" || tag == "latest"
}
