package dsl

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// ParseGitCredential decodes and validates a GitCredential YAML document.
func ParseGitCredential(r io.Reader) (*GitCredential, error) {
	var gc GitCredential
	dec := yaml.NewDecoder(r)
	if err := dec.Decode(&gc); err != nil {
		return nil, fmt.Errorf("parse GitCredential: %w", err)
	}
	if gc.Metadata.Name == "" {
		return nil, fmt.Errorf("GitCredential: metadata.name is required")
	}
	if gc.Spec.Host == "" {
		return nil, fmt.Errorf("GitCredential %q: spec.host is required", gc.Metadata.Name)
	}
	if gc.Spec.Type != "token" && gc.Spec.Type != "sshKey" {
		return nil, fmt.Errorf("GitCredential %q: spec.type must be \"token\" or \"sshKey\", got %q", gc.Metadata.Name, gc.Spec.Type)
	}
	if gc.Spec.SecretRef == "" {
		return nil, fmt.Errorf("GitCredential %q: spec.secretRef is required", gc.Metadata.Name)
	}
	return &gc, nil
}
