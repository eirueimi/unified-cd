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

// secretNameRe is the env-var-style name pattern that a secret name must match.
// It is the same character class the template engine recognizes for
// {{ secrets.NAME }} references (see template.go), so a name that validates
// here is always resolvable from a template.
var secretNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// ValidateSecretName checks name against the secret-name rule: alphanumerics,
// underscores, and hyphens, starting with a letter or underscore (e.g.
// AWS_KEY, github-token). This differs from ValidateName (DNS-1123) because
// secrets are referenced env-var style via {{ secrets.NAME }} and conventionally
// use uppercase/underscore names. Callers wrap the error with their field name.
func ValidateSecretName(name string) error {
	if name == "" {
		return fmt.Errorf("is required")
	}
	if !secretNameRe.MatchString(name) {
		return fmt.Errorf("is invalid: %q must consist of letters, digits, '_' or '-', and start with a letter or '_'", name)
	}
	return nil
}
