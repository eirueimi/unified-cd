package dsl

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// ParseWebhookReceiver decodes and validates a WebhookReceiver YAML from an io.Reader.
func ParseWebhookReceiver(r io.Reader) (*WebhookReceiver, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var wr WebhookReceiver
	if err := dec.Decode(&wr); err != nil {
		return nil, fmt.Errorf("decode yaml: %w", err)
	}
	if err := wr.Validate(); err != nil {
		return nil, err
	}
	return &wr, nil
}

// Validate validates the required fields and consistency of a WebhookReceiver.
func (wr *WebhookReceiver) Validate() error {
	if wr.APIVersion != SupportedAPIVersion {
		return fmt.Errorf("unsupported apiVersion %q", wr.APIVersion)
	}
	if wr.Kind != "WebhookReceiver" {
		return fmt.Errorf("unsupported kind %q (want \"WebhookReceiver\")", wr.Kind)
	}
	if err := ValidateName(wr.Metadata.Name); err != nil {
		return fmt.Errorf("metadata.name %w", err)
	}
	hasJob := wr.Spec.Trigger.Job != ""
	hasAppSource := wr.Spec.Trigger.AppSource != ""
	if hasJob == hasAppSource {
		return fmt.Errorf("spec.trigger must set exactly one of job or appSource")
	}
	switch wr.Spec.Auth.Type {
	case "none", "hmac-sha256", "github", "token":
	case "":
		wr.Spec.Auth.Type = "none"
	default:
		return fmt.Errorf("unsupported auth.type %q", wr.Spec.Auth.Type)
	}
	if wr.Spec.Auth.Type != "none" && wr.Spec.Auth.SecretRef == "" {
		return fmt.Errorf("spec.auth.secretRef is required for auth.type %q", wr.Spec.Auth.Type)
	}
	return nil
}
