package dsl

import (
	"fmt"
	"regexp"
)

var dns1123SubdomainRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

const dns1123SubdomainMaxLength = 253

// ValidateName checks name against the Kubernetes DNS-1123-subdomain rule:
// lowercase alphanumeric characters, '-', or '.', starting and ending with
// an alphanumeric character, max 253 characters. Empty input returns an
// "is required" error; format violations return an "is invalid: ..." error.
// Callers should wrap the returned error with their own field name, e.g.
// fmt.Errorf("metadata.name %w", err).
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("is required")
	}
	if len(name) > dns1123SubdomainMaxLength || !dns1123SubdomainRe.MatchString(name) {
		return fmt.Errorf("is invalid: %q must consist of lowercase alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character", name)
	}
	return nil
}
