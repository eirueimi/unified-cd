package dsl

import (
	"fmt"
	"io"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func ParseAppSource(r io.Reader) (*AppSource, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var as AppSource
	if err := dec.Decode(&as); err != nil {
		return nil, fmt.Errorf("decode yaml: %w", err)
	}
	if err := as.Validate(); err != nil {
		return nil, err
	}
	return &as, nil
}

func (a *AppSource) Validate() error {
	if a.APIVersion != SupportedAPIVersion {
		return fmt.Errorf("unsupported apiVersion %q (want %q)", a.APIVersion, SupportedAPIVersion)
	}
	if a.Kind != "AppSource" {
		return fmt.Errorf("unsupported kind %q (want \"AppSource\")", a.Kind)
	}
	if err := ValidateName(a.Metadata.Name); err != nil {
		return fmt.Errorf("metadata.name %w", err)
	}
	if a.Spec.RepoURL == "" {
		return fmt.Errorf("spec.repoURL is required")
	}
	if a.Spec.TargetRevision == "" {
		return fmt.Errorf("spec.targetRevision is required")
	}
	if a.Spec.Path == "" {
		return fmt.Errorf("spec.path is required")
	}
	if err := a.Spec.ValidateGitFields(); err != nil {
		return err
	}
	if strings.Contains(a.Spec.Path, "..") {
		return fmt.Errorf("spec.path must not contain ..")
	}
	if a.Spec.SyncPolicy.Interval != "" {
		d, err := time.ParseDuration(a.Spec.SyncPolicy.Interval)
		if err != nil {
			return fmt.Errorf("spec.syncPolicy.interval: %w", err)
		}
		if d < time.Minute {
			return fmt.Errorf("spec.syncPolicy.interval must be at least 1m")
		}
	}
	return nil
}
