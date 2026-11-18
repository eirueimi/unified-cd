package dsl

// AgentVolume declares a named volume that agents provision for jobs to mount
// (see the job's volumes field). The volume name is metadata.name.
//
// Every agent whose authorized labels include all of spec.agentSelector offers
// the volume; an empty selector means every agent.
type AgentVolume struct {
	APIVersion string          `yaml:"apiVersion" json:"apiVersion"`
	Kind       string          `yaml:"kind" json:"kind"`
	Metadata   Metadata        `yaml:"metadata" json:"metadata"`
	Spec       AgentVolumeSpec `yaml:"spec" json:"spec"`
}

type AgentVolumeSpec struct {
	// Mode is "exclusive" (one run at a time) or "shared" (concurrent runs).
	Mode string `yaml:"mode" json:"mode"`
	// Size is a Kubernetes quantity such as 200Gi. It must be positive.
	Size string `yaml:"size" json:"size"`
	// StorageClassName is the storage class for Kubernetes agents' PVCs.
	StorageClassName string `yaml:"storageClassName,omitempty" json:"storageClassName,omitempty"`
	// AgentSelector lists labels (e.g. role:unity) an agent must all carry to
	// offer this volume. Empty means every agent.
	AgentSelector []string `yaml:"agentSelector,omitempty" json:"agentSelector,omitempty"`
}
