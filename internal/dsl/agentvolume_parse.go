package dsl

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Volume modes for AgentVolume.spec.mode.
const (
	VolumeModeExclusive = "exclusive"
	VolumeModeShared    = "shared"
)

// Reclaim policies for AgentVolume.spec.reclaimPolicy. Empty means Retain.
const (
	ReclaimRetain = "Retain"
	ReclaimDelete = "Delete"
)

// ParseAgentVolume decodes and validates a kind: AgentVolume YAML document.
func ParseAgentVolume(r io.Reader) (*AgentVolume, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var v AgentVolume
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode yaml: %w", err)
	}
	if err := v.Validate(); err != nil {
		return nil, err
	}
	return &v, nil
}

// Validate validates the required fields of an AgentVolume manifest.
func (v *AgentVolume) Validate() error {
	if v.APIVersion != SupportedAPIVersion {
		return fmt.Errorf("unsupported apiVersion %q", v.APIVersion)
	}
	if v.Kind != "AgentVolume" {
		return fmt.Errorf("unsupported kind %q (want \"AgentVolume\")", v.Kind)
	}
	if v.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}
	// A DNS-1123 label is also a single path segment, which the volume's host
	// path (<workspaceDir>/volumes/<name>) relies on.
	if errs := validation.IsDNS1123Label(v.Metadata.Name); len(errs) > 0 {
		return fmt.Errorf("metadata.name is invalid: %q: %s", v.Metadata.Name, errs[0])
	}
	switch v.Spec.Mode {
	case VolumeModeExclusive, VolumeModeShared:
	case "":
		return fmt.Errorf("spec.mode is required (%q or %q)", VolumeModeExclusive, VolumeModeShared)
	default:
		return fmt.Errorf("spec.mode %q is invalid (want %q or %q)", v.Spec.Mode, VolumeModeExclusive, VolumeModeShared)
	}
	if v.Spec.Size == "" {
		return fmt.Errorf("spec.size is required")
	}
	if q, err := resource.ParseQuantity(v.Spec.Size); err != nil || q.Sign() <= 0 {
		return fmt.Errorf("spec.size %q must be a positive quantity such as 200Gi", v.Spec.Size)
	}
	for i, sel := range v.Spec.AgentSelector {
		if sel == "" {
			return fmt.Errorf("spec.agentSelector[%d] must not be empty", i)
		}
	}
	switch v.Spec.ReclaimPolicy {
	case "", ReclaimRetain, ReclaimDelete:
	default:
		return fmt.Errorf("spec.reclaimPolicy %q is invalid (want %q or %q)", v.Spec.ReclaimPolicy, ReclaimRetain, ReclaimDelete)
	}
	return nil
}

// AgentVolumeMatches reports whether an agent with agentLabels offers a volume
// with the given selector: every selector entry must be present in agentLabels.
// An empty selector matches every agent. It must agree with the claim query
// `agent_selector <@ agentLabels`.
func AgentVolumeMatches(selector, agentLabels []string) bool {
	have := make(map[string]bool, len(agentLabels))
	for _, l := range agentLabels {
		have[l] = true
	}
	for _, s := range selector {
		if !have[s] {
			return false
		}
	}
	return true
}
