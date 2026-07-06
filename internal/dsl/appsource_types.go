package dsl

import "time"

type AppSource struct {
	APIVersion string        `yaml:"apiVersion"`
	Kind       string        `yaml:"kind"`
	Metadata   Metadata      `yaml:"metadata"`
	Spec       AppSourceSpec `yaml:"spec"`
}

type AppSourceSpec struct {
	RepoURL        string        `yaml:"repoURL"`
	TargetRevision string        `yaml:"targetRevision"`
	Path           string        `yaml:"path"`
	SyncPolicy     AppSyncPolicy `yaml:"syncPolicy,omitempty"`
}

type AppSyncPolicy struct {
	Interval string `yaml:"interval,omitempty"`
	Prune    bool   `yaml:"prune,omitempty"`
	// AllowManualOverride disables the managed-resource write guard for
	// resources managed by this AppSource (direct apply/delete is allowed).
	AllowManualOverride bool `yaml:"allowManualOverride,omitempty"`
}

func (s AppSourceSpec) IntervalDuration() time.Duration {
	if s.SyncPolicy.Interval == "" {
		return 5 * time.Minute
	}
	d, _ := time.ParseDuration(s.SyncPolicy.Interval)
	return d
}
