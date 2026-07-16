package dsl

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// The JobTemplate type definitions live in jobtemplate_types.go so the
// schema/docs generators (cmd/schemagen, which scans *_types.go) pick them up.

// ParseJobTemplate strictly decodes and validates a kind: JobTemplate document.
// A kind: Job document gets an explicit conversion hint.
func ParseJobTemplate(data []byte) (*JobTemplate, error) {
	// Pre-sniff kind for a friendly error before the strict decode (a Job
	// document would otherwise fail on its first Job-only field, which is a
	// confusing message for what is really a wrong-kind problem).
	var head struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(data, &head); err != nil {
		return nil, err
	}
	if head.Kind == "Job" {
		return nil, fmt.Errorf("uses: targets must be kind: JobTemplate (got kind: Job); convert the template, or invoke the job with call:")
	}

	// The same forbidden-field pre-checks Job parsing applies (clear errors
	// for removed syntax like needs:).
	if err := checkForbiddenJobFields(data); err != nil {
		return nil, err
	}

	var tpl JobTemplate
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&tpl); err != nil {
		return nil, err
	}
	if err := tpl.Validate(); err != nil {
		return nil, err
	}
	return &tpl, nil
}

// Validate checks the JobTemplate's own invariants, reusing the step-level
// validation Job.Validate uses (native=false: a template is never native).
func (t *JobTemplate) Validate() error {
	if t.APIVersion != SupportedAPIVersion {
		return fmt.Errorf("unsupported apiVersion %q (want %q)", t.APIVersion, SupportedAPIVersion)
	}
	if t.Kind != "JobTemplate" {
		return fmt.Errorf("unsupported kind %q (want \"JobTemplate\")", t.Kind)
	}
	if t.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if err := ValidateName(t.Metadata.Name); err != nil {
		return fmt.Errorf("metadata.name %w", err)
	}
	if len(t.Spec.Steps) == 0 {
		return fmt.Errorf("spec.steps must contain at least one step")
	}
	if err := validShellArgv(t.Spec.Shell); err != nil {
		return fmt.Errorf("spec.shell: %w", err)
	}
	nameSet := map[string]bool{}
	if err := validateStepEntries(t.Spec.Steps, "spec.steps", nameSet, true, false); err != nil {
		return err
	}
	for i, p := range t.Spec.Params.Inputs {
		if p.Name == "" {
			return fmt.Errorf("spec.params.inputs[%d].name is required", i)
		}
		validTypes := map[string]bool{"string": true, "bool": true, "int": true, "array": true}
		if !validTypes[p.Type] {
			return fmt.Errorf("spec.params.inputs[%d].type %q is invalid (want string|bool|int|array)", i, p.Type)
		}
	}
	for i, o := range t.Spec.Params.Outputs {
		if o.Name == "" {
			return fmt.Errorf("spec.params.outputs[%d].name is required", i)
		}
		if o.Type == "" {
			return fmt.Errorf("spec.params.outputs[%d].type is required", i)
		}
	}
	return nil
}

// ToSpec converts the template into the dsl.Spec shape the uses: expansion
// consumes: the podTemplate subset becomes a regular PodTemplate whose Spec map
// carries only containers/volumes.
func (t *JobTemplate) ToSpec() Spec {
	spec := Spec{
		Params:      t.Spec.Params,
		Shell:       t.Spec.Shell,
		Steps:       t.Spec.Steps,
		Description: t.Spec.Description,
	}
	if pt := t.Spec.PodTemplate; pt != nil {
		m := map[string]any{}
		if len(pt.Spec.Containers) > 0 {
			list := make([]any, 0, len(pt.Spec.Containers))
			for _, c := range pt.Spec.Containers {
				list = append(list, c)
			}
			m["containers"] = list
		}
		if len(pt.Spec.Volumes) > 0 {
			list := make([]any, 0, len(pt.Spec.Volumes))
			for _, v := range pt.Spec.Volumes {
				list = append(list, v)
			}
			m["volumes"] = list
		}
		spec.PodTemplate = &PodTemplate{Spec: m}
	}
	return spec
}
