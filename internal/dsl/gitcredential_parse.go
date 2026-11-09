package dsl

import (
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseGitCredential decodes and validates a GitCredential YAML document.
func ParseGitCredential(r io.Reader) (*GitCredential, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var gc GitCredential
	if err := dec.Decode(&gc); err != nil {
		return nil, fmt.Errorf("decode yaml: %w", err)
	}
	if err := gc.Validate(); err != nil {
		return nil, err
	}
	return &gc, nil
}

// Validate validates the required fields of a GitCredential.
func (gc *GitCredential) Validate() error {
	if gc.APIVersion != SupportedAPIVersion {
		return fmt.Errorf("unsupported apiVersion %q (want %q)", gc.APIVersion, SupportedAPIVersion)
	}
	if gc.Kind != "GitCredential" {
		return fmt.Errorf("unsupported kind %q (want \"GitCredential\")", gc.Kind)
	}
	if err := ValidateName(gc.Metadata.Name); err != nil {
		return fmt.Errorf("metadata.name %w", err)
	}
	if gc.Spec.Host == "" {
		return fmt.Errorf("spec.host is required")
	}
	if gc.Spec.Type != "token" && gc.Spec.Type != "sshKey" {
		return fmt.Errorf("spec.type must be \"token\" or \"sshKey\", got %q", gc.Spec.Type)
	}
	if gc.Spec.SecretRef == "" {
		return fmt.Errorf("spec.secretRef is required")
	}
	if gc.Spec.KnownHosts != "" {
		if gc.Spec.Type != "sshKey" {
			return fmt.Errorf("spec.knownHosts is only valid for type \"sshKey\"")
		}
		if err := ValidateKnownHosts(gc.Spec.KnownHosts); err != nil {
			return fmt.Errorf("spec.%w", err)
		}
	}
	return nil
}

// ValidateKnownHosts checks that s is a plausible OpenSSH known_hosts block: blank
// lines and "#" comments are allowed, every other line needs at least a host
// pattern, a key type and a base64 key (an optional leading "@cert-authority" or
// "@revoked" marker is accepted). It does not verify the key material itself.
func ValidateKnownHosts(s string) error {
	n := 0
	for i, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if strings.HasPrefix(fields[0], "@") {
			fields = fields[1:]
		}
		if len(fields) < 3 {
			return fmt.Errorf("knownHosts line %d: want \"<host> <key-type> <base64-key>\"", i+1)
		}
		n++
	}
	if n == 0 {
		return fmt.Errorf("knownHosts has no key lines")
	}
	return nil
}
