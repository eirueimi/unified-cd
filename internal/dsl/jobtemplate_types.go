package dsl

// JobTemplate is the resource a uses: step points at. Unlike a full Job, its
// schema contains ONLY what uses: can honor — the template's steps are inlined
// into the CALLER's run and pod, so fields that would shape a different pod,
// agent, or run (agentSelector, concurrency, timeoutMinutes, native, finally,
// podTemplate reuse/workspace/override, pod-level spec keys) do not exist here
// and are rejected by strict decoding. A job that needs its own pod/agent/run
// semantics should be invoked with call: instead.
type JobTemplate struct {
	APIVersion string          `yaml:"apiVersion"`
	Kind       string          `yaml:"kind"`
	Metadata   Metadata        `yaml:"metadata"`
	Spec       JobTemplateSpec `yaml:"spec"`
}

// JobTemplateSpec is the uses:-supported subset of a job spec.
type JobTemplateSpec struct {
	Description string                  `yaml:"description,omitempty"`
	Params      Params                  `yaml:"params,omitempty"`
	Shell       []string                `yaml:"shell,omitempty"`
	PodTemplate *JobTemplatePodTemplate `yaml:"podTemplate,omitempty"`
	Steps       []StepEntry             `yaml:"steps"`
}

// JobTemplatePodTemplate is the pod-shape subset a template may contribute to
// the caller's pod: containers and the volumes they mount. Nothing else.
type JobTemplatePodTemplate struct {
	Spec JobTemplatePodSpec `yaml:"spec,omitempty"`
}

// JobTemplatePodSpec holds the mergeable pod-shape lists.
type JobTemplatePodSpec struct {
	Containers []map[string]any `yaml:"containers,omitempty"`
	Volumes    []map[string]any `yaml:"volumes,omitempty"`
}
