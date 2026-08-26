package dsl

// Vars is a global variable manifest: plain-text values shared by every job.
//
// These are NOT secrets. Values are stored in the clear, returned in the clear,
// and printed in step logs like any other environment variable. A value that
// must not appear in a log belongs in a Secret.
type Vars struct {
	APIVersion string   `yaml:"apiVersion" json:"apiVersion"`
	Kind       string   `yaml:"kind" json:"kind"`
	Metadata   Metadata `yaml:"metadata" json:"metadata"`
	Spec       VarsSpec `yaml:"spec" json:"spec"`
}

type VarsSpec struct {
	Vars map[string]string `yaml:"vars" json:"vars"`
}
