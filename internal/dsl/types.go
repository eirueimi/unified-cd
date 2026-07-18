package dsl

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type Job struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

type Metadata struct {
	Name        string            `yaml:"name"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
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
	// Native opts the whole job into host-process execution (no claim pod,
	// no podTemplate, no container: steps). Host agents only; the default
	// (false) is the isolated pod model on both backends.
	Native bool `yaml:"native,omitempty" json:"native,omitempty"`
	// Shell overrides the default interpreter argv for every step in this
	// job that does not declare its own step-level shell:. Array-only (no
	// scalar shorthand); the run: script is appended as the final argv
	// element. See Step.Shell for the full resolution priority.
	Shell []string `yaml:"shell,omitempty" json:"shell,omitempty"`
	// Description is a human-readable summary of the job, shown in the WebUI.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
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
	// Pattern is a regular expression every supplied value must match (defaults
	// are checked too, so a bad default cannot slip through). Param values are
	// interpolated into step shell text, so a param fed from an untrusted
	// source — a webhook payload especially — is a command-injection vector
	// unless constrained. Suggested starting point: ^[A-Za-z0-9._/-]+$
	Pattern string `yaml:"pattern,omitempty"`
	// Unvalidated explicitly opts this input out of the pattern requirement for
	// payload-mapped params. Use only when the value is genuinely free-form and
	// never reaches a shell.
	Unvalidated bool `yaml:"unvalidated,omitempty"`
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
	RunsIn           *RunsIn               `yaml:"runsIn,omitempty" json:"runsIn,omitempty"`
	// Scope tagging: set by inline expansion when a uses-level runsIn.image
	// makes the whole template one isolated scope. Steps sharing ScopeID run
	// in one environment. Not user-authored.
	ScopeID        string      `yaml:"scopeID,omitempty" json:"scopeID,omitempty"`
	ScopeImage     string      `yaml:"scopeImage,omitempty" json:"scopeImage,omitempty"`
	TimeoutMinutes float64     `yaml:"timeoutMinutes,omitempty"`
	Retry          *RetrySpec  `yaml:"retry,omitempty" json:"retry,omitempty"`
	Foreach        *ForeachDef `yaml:"foreach,omitempty"`
	Matrix         *MatrixDef  `yaml:"matrix,omitempty"`
	// Shell overrides the effective interpreter argv for this step. See
	// Step.Shell for the full resolution priority.
	Shell []string `yaml:"shell,omitempty" json:"shell,omitempty"`

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
	RunsIn           *RunsIn               `yaml:"runsIn,omitempty" json:"runsIn,omitempty"`
	// Scope tagging: set by inline expansion when a uses-level runsIn.image
	// makes the whole template one isolated scope. Steps sharing ScopeID run
	// in one environment. Not user-authored.
	ScopeID        string      `yaml:"scopeID,omitempty" json:"scopeID,omitempty"`
	ScopeImage     string      `yaml:"scopeImage,omitempty" json:"scopeImage,omitempty"`
	TimeoutMinutes float64     `yaml:"timeoutMinutes,omitempty"`
	Retry          *RetrySpec  `yaml:"retry,omitempty" json:"retry,omitempty"`
	Foreach        *ForeachDef `yaml:"foreach,omitempty"`
	Matrix         *MatrixDef  `yaml:"matrix,omitempty"`
	// Shell overrides the effective interpreter argv for this step. Array
	// form only (v1): e.g. [bash, -lc] or [python3, -c]; the run: script is
	// appended as the final argv element. Resolution priority (most specific
	// wins): step.shell > a uses: template's own declared shell > spec.shell
	// (job-level) > system default. Steps inside parallel: and finally:
	// count as steps for this purpose.
	Shell []string `yaml:"shell,omitempty" json:"shell,omitempty"`
	// Needs removed — use parallel: blocks instead
}

// RetrySpec configures automatic re-runs of a failing run: step.
type RetrySpec struct {
	// Attempts is the total number of tries (1 = no retry). Must be >= 1.
	Attempts int `yaml:"attempts" json:"attempts"`
	// Backoff is a fixed wait between tries as a Go duration (e.g. "30s").
	// Empty means 0 (immediate retry).
	Backoff string `yaml:"backoff,omitempty" json:"backoff,omitempty"`
}

// ForeachDef expands a step into one parallel run per item in the list.
// Key is the variable name accessible in templates as {{ .Foreach.key }}.
type ForeachDef struct {
	Key    string        `yaml:"key"`
	Source ForeachSource `yaml:"in"`
}

// MatrixDef expands a step into one copy per combination of dimension values
// (cartesian product minus exclude entries). Dimensions preserve YAML
// declaration order; the combination key joins values with "/" in that order
// (e.g. "linux/amd64").
type MatrixDef struct {
	Dimensions []MatrixDimension
	Exclude    []map[string]string
}

type MatrixDimension struct {
	Name   string
	Source ForeachSource
}

// RunsIn declares the execution context for a uses: template entry. It is no
// longer legal on a plain step (step-level runsIn: was removed; the flat
// container: field is the canonical way to pin a plain step to a podTemplate
// container). On a uses: entry, only the image form is accepted: it declares
// that the whole inlined template runs in one fresh isolated scope built from
// this image (host: `<rt> run`; k8s: a throwaway pod). No workspace is shared
// — pass inputs via with:/env, return outputs via outputs:/stdout.
// runsIn.container on a uses: entry is rejected; set container: on the
// template's own steps instead.
type RunsIn struct {
	Image     string        `yaml:"image,omitempty" json:"image,omitempty"`
	Container string        `yaml:"container,omitempty" json:"container,omitempty"`
	Resources *ResourceSpec `yaml:"resources,omitempty" json:"resources,omitempty"`
}

// ResourceSpec declares CPU/memory requests and limits for a runsIn.image step.
type ResourceSpec struct {
	Requests *ResourceList `yaml:"requests,omitempty" json:"requests,omitempty"`
	Limits   *ResourceList `yaml:"limits,omitempty" json:"limits,omitempty"`
}

// ResourceList is a cpu/memory pair using Kubernetes quantity strings
// (e.g. "500m", "1", "256Mi", "1Gi").
type ResourceList struct {
	CPU    string `yaml:"cpu,omitempty" json:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"`
}

// UnmarshalYAML parses the matrix mapping while preserving key order.
// The reserved key "exclude" holds combination filters; every other key is a
// dimension whose value is a ForeachSource (list or expression string).
func (m *MatrixDef) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("matrix must be a mapping of dimension name to a list or expression")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, valNode := node.Content[i], node.Content[i+1]
		if keyNode.Value == "exclude" {
			if err := valNode.Decode(&m.Exclude); err != nil {
				return fmt.Errorf("matrix.exclude: %w", err)
			}
			continue
		}
		var src ForeachSource
		if err := valNode.Decode(&src); err != nil {
			return fmt.Errorf("matrix.%s: %w", keyNode.Value, err)
		}
		m.Dimensions = append(m.Dimensions, MatrixDimension{Name: keyNode.Value, Source: src})
	}
	return nil
}

// MarshalYAML emits the same mapping form UnmarshalYAML accepts — each
// dimension as "name: source" in declaration order, then the reserved
// "exclude" key — so specs round-trip through yaml.Marshal (the job-YAML
// endpoint and `unified-cd export`).
func (m MatrixDef) MarshalYAML() (any, error) {
	node := &yaml.Node{Kind: yaml.MappingNode}
	for _, d := range m.Dimensions {
		var val yaml.Node
		if err := val.Encode(d.Source); err != nil {
			return nil, fmt.Errorf("matrix.%s: %w", d.Name, err)
		}
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: d.Name},
			&val)
	}
	if len(m.Exclude) > 0 {
		var val yaml.Node
		if err := val.Encode(m.Exclude); err != nil {
			return nil, fmt.Errorf("matrix.exclude: %w", err)
		}
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "exclude"},
			&val)
	}
	return node, nil
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

// MarshalYAML emits the sequence-or-string form UnmarshalYAML accepts:
// a plain string for an expression, otherwise the literal list.
func (f ForeachSource) MarshalYAML() (any, error) {
	if f.Expr != "" {
		return f.Expr, nil
	}
	return f.Literal, nil
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
	// Shell overrides the interpreter argv for this post hook. When absent,
	// the hook inherits its owning step's effective shell. The override
	// exists because inheritance alone breaks down for non-shell
	// interpreters: a step running under shell: [python3, -c] with a
	// shell-script cleanup hook needs post: {shell: [sh, -c], run: ...} to
	// be expressible at all.
	Shell []string `yaml:"shell,omitempty"`
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

// InputDefaultsAsStrings returns a map of input name to stringified default
// value, for every declared input that carries a non-nil default. Inputs with
// a nil Default (no `default:` in the YAML) are omitted from the result rather
// than rendered as the literal "<nil>".
//
// It uses the same value-to-string conversion as with: (see stringifyValue),
// so a template's uses: (inline) and call: (child-run) paths stringify a given
// default identically.
func (p Params) InputDefaultsAsStrings() map[string]string {
	if len(p.Inputs) == 0 {
		return nil
	}
	result := make(map[string]string, len(p.Inputs))
	for _, in := range p.Inputs {
		if in.Default == nil {
			continue
		}
		result[in.Name] = stringifyValue(in.Default)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// withAsStrings is the shared conversion used by CallStep.WithAsStrings and
// UsesStep.WithAsStrings.
func withAsStrings(with map[string]any) map[string]string {
	if len(with) == 0 {
		return nil
	}
	result := make(map[string]string, len(with))
	for k, v := range with {
		result[k] = stringifyValue(v)
	}
	return result
}

// stringifyValue converts a single YAML-decoded value (string, []any, or other
// scalar) to its string form. []any (YAML array) values are joined with
// newlines; other scalars are converted via fmt.Sprintf. Shared by
// withAsStrings (with: values) and InputDefaultsAsStrings (input defaults) so
// both stringify the same way.
func stringifyValue(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case []any:
		parts := make([]string, len(val))
		for i, item := range val {
			parts[i] = fmt.Sprintf("%v", item)
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprintf("%v", val)
	}
}

type CacheStep struct {
	Path        string   `yaml:"path"`                  // directory to cache; supports template expansion
	Key         string   `yaml:"key"`                   // cache key; supports template expansion
	RestoreKeys []string `yaml:"restoreKeys,omitempty"` // fallback key prefixes; support template expansion
	TTLDays     int      `yaml:"ttlDays,omitempty"`     // default 30, max 365
}

type PodTemplate struct {
	Name           string           `yaml:"name,omitempty" json:"name,omitempty"`
	Reuse          bool             `yaml:"reuse,omitempty" json:"reuse,omitempty"`
	CleanWorkspace bool             `yaml:"cleanWorkspace,omitempty" json:"cleanWorkspace,omitempty"`
	Workspace      *WorkspaceConfig `yaml:"workspace,omitempty" json:"workspace,omitempty"`
	Override       *PodSpecPatch    `yaml:"override,omitempty" json:"override,omitempty"`
	Spec           map[string]any   `yaml:"spec,omitempty" json:"spec,omitempty"`
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
