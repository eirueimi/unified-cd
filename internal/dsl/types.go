package dsl

import (
	"fmt"
	"strings"
)

type Job struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

type Metadata struct {
	Name   string            `yaml:"name"`
	Labels map[string]string `yaml:"labels,omitempty"`
}

type Spec struct {
	Params        Params       `yaml:"params"`
	Concurrency   *Concurrency `yaml:"concurrency,omitempty"`
	AgentSelector []string     `yaml:"agentSelector,omitempty"`
	// Steps is the main DAG of steps to execute.
	// (failFast was removed — all started steps run to completion.)
	Steps []StepEntry `yaml:"steps"`
	// Finally runs after the main DAG completes, on success, failure, or
	// cancellation. Same structure as Steps. A finally step's `if:` defaults to
	// always-run; use if: failure()/success() to filter. A finally step that
	// fails marks the run Failed (after all finally steps run).
	Finally        []StepEntry  `yaml:"finally,omitempty"`
	TimeoutMinutes float64      `yaml:"timeoutMinutes,omitempty"`
	PodTemplate    *PodTemplate `yaml:"podTemplate,omitempty"`
}

type Params struct {
	Inputs  []Input  `yaml:"inputs,omitempty"`
	Outputs []Output `yaml:"outputs,omitempty"`
}

type Input struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type" schema:"enum:string,bool,int,array"`
	Required    bool   `yaml:"required,omitempty"`
	Default     any    `yaml:"default,omitempty"`
	Description string `yaml:"description,omitempty"`
}

type Output struct {
	Name string `yaml:"name"`
	Type string `yaml:"type" schema:"enum:string,bool,int,artifact"` // "string", "bool", "int", "artifact"
}

type Concurrency struct {
	Mutex      string      `yaml:"mutex,omitempty"`
	Semaphores []Semaphore `yaml:"semaphores,omitempty"`
	OrLocks    []OrLock    `yaml:"orLocks,omitempty"`
}

type Semaphore struct {
	Pool     string `yaml:"pool"`
	Capacity int    `yaml:"capacity"`
}

// OrLock acquires exactly one candidate from In — whichever is free — instead of
// requiring all of them like Semaphores does. The acquired candidate value is
// exposed to the Job's steps as a synthesized parameter named
// strings.ToUpper(Name)+"_LOCK_VALUE" (e.g. Name "env" -> "ENV_LOCK_VALUE"),
// readable via {{ .Params.ENV_LOCK_VALUE }}.
type OrLock struct {
	Name string        `yaml:"name"`
	In   ForeachSource `yaml:"in" json:"in"`
}

// StepEntry is either a concrete step (Name is set) or a parallel group (Parallel is set).
// The two forms are mutually exclusive; Validate enforces this.
type StepEntry struct {
	// Concrete step fields (identical to Step, minus Needs)
	Name             string                `yaml:"name,omitempty"`
	If               string                `yaml:"if,omitempty"`
	Env              map[string]string     `yaml:"env,omitempty"`
	Run              string                `yaml:"run,omitempty"`
	Outputs          map[string]string     `yaml:"outputs,omitempty"`
	Call             *CallStep             `yaml:"call,omitempty"`
	Uses             *UsesStep             `yaml:"uses,omitempty"`
	Cache            *CacheStep            `yaml:"cache,omitempty"`
	UploadArtifact   *UploadArtifactStep   `yaml:"uploadArtifact,omitempty"`
	DownloadArtifact *DownloadArtifactStep `yaml:"downloadArtifact,omitempty"`
	Approval         *ApprovalStep         `yaml:"approval,omitempty"`
	Post             *PostStep             `yaml:"post,omitempty"`
	ContinueOnError  bool                  `yaml:"continueOnError,omitempty"`
	Container        string                `yaml:"container,omitempty"`
	TimeoutMinutes   float64               `yaml:"timeoutMinutes,omitempty"`
	Foreach          *ForeachDef           `yaml:"foreach,omitempty"`

	// Parallel group (mutually exclusive with all concrete step fields above)
	Parallel []Step `yaml:"parallel,omitempty"`
}

// Step is a concrete step. Used inside parallel: blocks and as the body of a StepEntry.
type Step struct {
	Name             string                `yaml:"name"`
	If               string                `yaml:"if,omitempty"`
	Env              map[string]string     `yaml:"env,omitempty"`
	Run              string                `yaml:"run,omitempty"`
	Outputs          map[string]string     `yaml:"outputs,omitempty"`
	Call             *CallStep             `yaml:"call,omitempty"`
	Uses             *UsesStep             `yaml:"uses,omitempty"`
	Cache            *CacheStep            `yaml:"cache,omitempty"`
	UploadArtifact   *UploadArtifactStep   `yaml:"uploadArtifact,omitempty"`
	DownloadArtifact *DownloadArtifactStep `yaml:"downloadArtifact,omitempty"`
	Approval         *ApprovalStep         `yaml:"approval,omitempty"`
	Post             *PostStep             `yaml:"post,omitempty"`
	ContinueOnError  bool                  `yaml:"continueOnError,omitempty"`
	Container        string                `yaml:"container,omitempty"`
	TimeoutMinutes   float64               `yaml:"timeoutMinutes,omitempty"`
	Foreach          *ForeachDef           `yaml:"foreach,omitempty"`
	// Needs removed — use parallel: blocks instead
}

// ForeachDef expands a step into one parallel run per item in the list.
// Key is the variable name accessible in templates as {{ .Foreach.key }}.
type ForeachDef struct {
	Key    string        `yaml:"key"`
	Source ForeachSource `yaml:"in"`
}

// ForeachSource is either a literal list (YAML sequence) or a template expression (YAML string).
//
//   in: [prod, staging, dev]                    → Literal
//   in: $envs                                   → Expr (JSON-array param reference)
//   in: "{{ .Params.envs | split \",\" }}"      → Expr (template)
//   in: "{{ .Steps.list.Outputs.envs | split \",\" }}" → Expr (step output reference)
type ForeachSource struct {
	Literal []string `json:"literal,omitempty"`
	Expr    string   `json:"expr,omitempty"`
}

// UnmarshalYAML handles the sequence-or-string ambiguity.
func (f *ForeachSource) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var list []string
	if err := unmarshal(&list); err == nil {
		f.Literal = list
		return nil
	}
	var s string
	if err := unmarshal(&s); err != nil {
		return fmt.Errorf("foreach.in must be a list or a string expression: %w", err)
	}
	f.Expr = s
	return nil
}

type UploadArtifactStep struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

type DownloadArtifactStep struct {
	Name    string `yaml:"name"`
	DestDir string `yaml:"destDir,omitempty"` // defaults to the current directory if omitted
}

// ApprovalStep pauses the run until an authenticated user approves or rejects.
// TimeoutMinutes defaults to 60 (applied at compile time) when zero.
type ApprovalStep struct {
	Message        string  `yaml:"message,omitempty"`
	TimeoutMinutes float64 `yaml:"timeoutMinutes,omitempty"`
}

// PostStep defines cleanup/post-processing to run after a step completes.
// Executed in LIFO order after RunDAG completes.
type PostStep struct {
	Run string            `yaml:"run,omitempty"`
	Env map[string]string `yaml:"env,omitempty"`
}

type CallStep struct {
	Job  string         `yaml:"job"`
	With map[string]any `yaml:"with,omitempty"`
}

// WithAsStrings converts With values to map[string]string.
// []interface{} (YAML array) values are joined with newlines.
// Other scalar values are converted via fmt.Sprintf.
func (c *CallStep) WithAsStrings() map[string]string {
	return withAsStrings(c.With)
}

// UsesStep inlines a git-template job's steps directly into the current run.
// Job must be a git:// URI; unlike CallStep, it never references a registered job name.
type UsesStep struct {
	Job  string         `yaml:"job"`
	With map[string]any `yaml:"with,omitempty"`
}

// WithAsStrings converts With values to map[string]string. See CallStep.WithAsStrings.
func (u *UsesStep) WithAsStrings() map[string]string {
	return withAsStrings(u.With)
}

// withAsStrings is the shared conversion used by CallStep.WithAsStrings and
// UsesStep.WithAsStrings.
func withAsStrings(with map[string]any) map[string]string {
	if len(with) == 0 {
		return nil
	}
	result := make(map[string]string, len(with))
	for k, v := range with {
		switch val := v.(type) {
		case string:
			result[k] = val
		case []any:
			parts := make([]string, len(val))
			for i, item := range val {
				parts[i] = fmt.Sprintf("%v", item)
			}
			result[k] = strings.Join(parts, "\n")
		default:
			result[k] = fmt.Sprintf("%v", val)
		}
	}
	return result
}

type CacheStep struct {
	Path        string   `yaml:"path"`                  // directory to cache; supports template expansion
	Key         string   `yaml:"key"`                   // cache key; supports template expansion
	RestoreKeys []string `yaml:"restoreKeys,omitempty"` // fallback key prefixes; support template expansion
	TTLDays     int      `yaml:"ttlDays,omitempty"`     // default 30
}

type PodTemplate struct {
	Name           string                 `yaml:"name,omitempty" json:"name,omitempty"`
	Reuse          bool                   `yaml:"reuse,omitempty" json:"reuse,omitempty"`
	CleanWorkspace bool                   `yaml:"cleanWorkspace,omitempty" json:"cleanWorkspace,omitempty"`
	Workspace      *WorkspaceConfig       `yaml:"workspace,omitempty" json:"workspace,omitempty"`
	Override       *PodSpecPatch          `yaml:"override,omitempty" json:"override,omitempty"`
	Spec           map[string]any `yaml:"spec,omitempty" json:"spec,omitempty"`
}

type WorkspaceConfig struct {
	MountPath string        `yaml:"mountPath,omitempty" json:"mountPath,omitempty"`
	PVC       *WorkspacePVC `yaml:"pvc,omitempty" json:"pvc,omitempty"`
}

type WorkspacePVC struct {
	ClaimName        string `yaml:"claimName,omitempty" json:"claimName,omitempty"`
	StorageClassName string `yaml:"storageClassName,omitempty" json:"storageClassName,omitempty"`
	StorageRequest   string `yaml:"storageRequest,omitempty" json:"storageRequest,omitempty"`
	AccessMode       string `yaml:"accessMode,omitempty" json:"accessMode,omitempty" schema:"enum:ReadWriteOnce,ReadOnlyMany,ReadWriteMany"`
}

type PodSpecPatch struct {
	Containers []map[string]any `yaml:"containers,omitempty" json:"containers,omitempty"`
	Volumes    []map[string]any `yaml:"volumes,omitempty" json:"volumes,omitempty"`
}
