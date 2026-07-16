package dsl

import (
	"fmt"
	"strings"
	"time"
)

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

// ValidateGitFields validates the fields of an AppSourceSpec that flow into a
// git subprocess argv: the repo URL, the target revision (ref), and the path.
// It rejects anything that could be interpreted as a git command-line option
// or an unsupported/dangerous transport (git option injection).
//
// This is the SINGLE implementation of that check, shared by two call sites:
//   - AppSource.Validate, at apply time (a user submitting/applying an
//     AppSource document).
//   - The AppSource reconciler's read-path (syncAppSource), which re-validates
//     specs loaded back out of the store before they are ever used to build a
//     git argv. Apply-time validation alone is not sufficient: legacy rows
//     written before this validation existed, or rows inserted directly against
//     the store (bypassing ParseAppSource/Validate), would otherwise reach the
//     git-exec sites unchecked.
func (s AppSourceSpec) ValidateGitFields() error {
	if err := ValidateGitRepoURL(s.RepoURL); err != nil {
		return fmt.Errorf("spec.repoURL: %w", err)
	}
	if err := ValidateGitRef(s.TargetRevision); err != nil {
		return fmt.Errorf("spec.targetRevision: %w", err)
	}
	if strings.HasPrefix(s.Path, "-") {
		return fmt.Errorf("spec.path %q must not start with '-'", s.Path)
	}
	return nil
}
