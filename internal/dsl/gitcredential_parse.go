package dsl

import (
	"fmt"
	"io"

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
	return nil
}
