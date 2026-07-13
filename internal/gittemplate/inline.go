package gittemplate

import (
	"fmt"
	"regexp"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

const usesPrefixSep = "__"

// paramRefGoRe matches Go-template ".Params.NAME" references.
var paramRefGoRe = regexp.MustCompile(`\.Params\.([A-Za-z_][A-Za-z0-9_]*)`)

// paramRefCondRe matches condition-language "params.NAME" references (used in if:).
var paramRefCondRe = regexp.MustCompile(`\bparams\.([A-Za-z_][A-Za-z0-9_]*)`)

// stepRefGoRe matches Go-template ".Steps.NAME.Outputs." references, capturing NAME.
var stepRefGoRe = regexp.MustCompile(`\.Steps\.([A-Za-z_][A-Za-z0-9_]*)\.Outputs\.`)

// stepRefCondRe matches condition-language "steps.NAME.outputs." references, capturing NAME.
var stepRefCondRe = regexp.MustCompile(`\bsteps\.([A-Za-z_][A-Za-z0-9_]*)\.outputs\.`)

// checkScopeStepAllowed rejects step shapes that don't make sense inside a
// scoped uses (uses-level runsIn.image, i.e. a single shared environment):
//   - container: the scope IS the environment, a step can't declare a second
//     exec target.
//   - approval: would hold the isolated scope environment (container/pod)
//     alive across a human wait, wasting resources and risking the k8s pod
//     deadline killing it mid-wait.
//   - call: spawns a separate child run on another agent/workspace that
//     cannot see the scope's isolated filesystem, so it has undefined
//     semantics inside a scope.
//
// Template step-level runsIn: is rejected earlier and unconditionally (in both
// scope and non-scope mode) — this function only guards the exec-target field
// that can still legally appear on a template step, container:.
func checkScopeStepAllowed(name string, container string, hasApproval, hasCall bool) error {
	if container != "" {
		return fmt.Errorf("step %q: container: is not allowed inside a uses running with runsIn.image (the scope is a single environment)", name)
	}
	if hasApproval {
		return fmt.Errorf("step %q: approval is not allowed inside a uses running with runsIn.image (it would hold the scope environment alive across a human wait)", name)
	}
	if hasCall {
		return fmt.Errorf("step %q: call is not allowed inside a uses running with runsIn.image (a called job cannot see the scope's isolated filesystem)", name)
	}
	return nil
}

// ExpandUsesStep is the exported form of expandUsesStep, for callers outside
// this package (e.g. internal/controller) that need to inline a uses:
// template's steps directly — such as verifying shell: composition
// end-to-end against api.ClaimStep.
func ExpandUsesStep(usesName string, with map[string]string, tplSpec dsl.Spec, outerRunsIn *dsl.RunsIn, outerContainer string) ([]dsl.StepEntry, error) {
	return expandUsesStep(usesName, with, tplSpec, outerRunsIn, outerContainer)
}

// unsafeNameChar matches any character that isn't a Go-template identifier
// char. Synthetic step names are embedded verbatim in {{ .Steps.<name>.Outputs.X }}
// selectors, and a hyphen (or any non-identifier char) makes that an
// unparseable template — the whole action then fails to render and .Params
// values come out empty. safeName maps such chars to '_' so the generated
// names (and the refs that point at them, built from the same helpers) stay
// valid selectors regardless of the user's chosen step names.
var unsafeNameChar = regexp.MustCompile(`[^A-Za-z0-9_]`)

func safeName(s string) string { return unsafeNameChar.ReplaceAllString(s, "_") }

func prefixedName(usesName, innerName string) string {
	return safeName(usesName) + usesPrefixSep + safeName(innerName)
}

func inputsStepName(usesName string) string {
	return safeName(usesName) + usesPrefixSep + "inputs"
}

// scopeIDFor returns the scope identity shared by all steps expanded from a
// uses-level runsIn.image invocation. The agent keys the scope environment on
// (ScopeID, MatrixKey) so matrix variants get independent environments.
func scopeIDFor(usesName string) string { return "scope:" + usesName }

// rewriteRefs rewrites .Params.X / params.X to point at the synthetic inputs-capture
// step, and .Steps.<inner>.Outputs.X / steps.<inner>.outputs.X — where <inner> is one
// of the template job's own step names — to point at that step's prefixed name.
// References to step names outside innerNames are left untouched.
func rewriteRefs(s, usesName string, innerNames map[string]bool) string {
	if s == "" {
		return s
	}
	inputs := inputsStepName(usesName)
	s = paramRefGoRe.ReplaceAllString(s, ".Steps."+inputs+".Outputs.$1")
	s = paramRefCondRe.ReplaceAllString(s, "steps."+inputs+".outputs.$1")
	s = stepRefGoRe.ReplaceAllStringFunc(s, func(m string) string {
		name := stepRefGoRe.FindStringSubmatch(m)[1]
		if !innerNames[name] {
			return m
		}
		return ".Steps." + prefixedName(usesName, name) + ".Outputs."
	})
	s = stepRefCondRe.ReplaceAllStringFunc(s, func(m string) string {
		name := stepRefCondRe.FindStringSubmatch(m)[1]
		if !innerNames[name] {
			return m
		}
		return "steps." + prefixedName(usesName, name) + ".outputs."
	})
	return s
}

func rewriteMap(m map[string]string, usesName string, innerNames map[string]bool) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = rewriteRefs(v, usesName, innerNames)
	}
	return out
}

// expandUsesStep replaces a single `uses` step with a flat, sequenced step
// list that can be spliced directly into the parent spec's Steps in its place:
//
//	<usesName>__inputs              synthetic: captures tplSpec.Params.Inputs defaults
//	                                 overlaid by `with` (evaluated in the parent's
//	                                 runtime context at this position)
//	<usesName>__<innerStep...>       tplSpec's own steps, renamed + reference-rewritten
//	<usesName>                       synthetic: captures tplSpec's declared
//	                                 spec.params.outputs (always present, even if empty)
//
// Order is sequential (array position); Needs is no longer used.
// with is the already-stringified `uses.with` map (via UsesStep.WithAsStrings).
// Declared input defaults (tplSpec.Params.Inputs[i].Default) are seeded into the
// __inputs step first and then overridden by a matching non-empty with: entry,
// mirroring call:'s resolveParams defaulting behavior — like resolveParams, an
// explicit empty-string with: value counts as unset (it falls back to a declared
// default and does not satisfy a required input). A declared-required input with
// no default and no non-empty with: entry is a hard error (see the missing-inputs
// check below) instead of silently rendering "<no value>" inside the inlined steps.
// tplSpec must have at least one step (the caller is responsible for having already
// validated the fetched job, e.g. via dsl.Job.Validate()).
// outerRunsIn is the RunsIn declared on the outer `uses` step (may be nil); per
// Task 1's DSL rules this can only be image-only (runsIn.image), which puts the
// whole expansion in scope mode. outerContainer is the outer `uses` step's flat
// container: field (may be ""); in non-scope mode each inlined template step
// keeps its own container: if set, otherwise inherits outerContainer. A template
// step that still carries runsIn: is always rejected — step-level runsIn: was
// removed in favor of container:.
func expandUsesStep(usesName string, with map[string]string, tplSpec dsl.Spec, outerRunsIn *dsl.RunsIn, outerContainer string) ([]dsl.StepEntry, error) {
	if len(tplSpec.Steps) == 0 {
		return nil, fmt.Errorf("template job has no steps")
	}

	scopeMode := outerRunsIn != nil && outerRunsIn.Image != ""
	var scopeID, scopeImage string
	if scopeMode {
		scopeID = scopeIDFor(usesName)
		scopeImage = outerRunsIn.Image
	}

	innerNames := make(map[string]bool, len(tplSpec.Steps))
	for _, s := range tplSpec.Steps {
		if s.Name != "" {
			innerNames[s.Name] = true
		}
		for _, p := range s.Parallel {
			if p.Name != "" {
				innerNames[p.Name] = true
			}
		}
	}

	// Required inputs mirror call:'s resolveParams semantics (internal/controller/params.go):
	// a required input with no default and no explicit non-empty with: value is
	// an error, not a silent "<no value>" render. Like resolveParams, an
	// explicit empty string counts as unset.
	var missing []string
	for _, in := range tplSpec.Params.Inputs {
		if !in.Required || in.Default != nil {
			continue
		}
		if v, ok := with[in.Name]; ok && v != "" {
			continue
		}
		missing = append(missing, in.Name)
	}
	if len(missing) == 1 {
		return nil, fmt.Errorf("uses %q: missing required input: %s", usesName, missing[0])
	}
	if len(missing) > 1 {
		return nil, fmt.Errorf("uses %q: missing required inputs: %v", usesName, missing)
	}

	// Seed the inputs map from the template's declared input defaults, then
	// overlay the explicit with: values — a non-empty with: value always wins
	// over a default, but (mirroring resolveParams) an explicit empty string is
	// treated as unset and falls back to the declared default when one exists.
	defaults := tplSpec.Params.InputDefaultsAsStrings()
	inputsOutputs := make(map[string]string, len(with)+len(tplSpec.Params.Inputs))
	for k, v := range defaults {
		inputsOutputs[k] = v
	}
	for k, v := range with {
		if v == "" {
			if _, hasDefault := defaults[k]; hasDefault {
				continue
			}
		}
		inputsOutputs[k] = v
	}
	inputsStep := dsl.StepEntry{
		Name:    inputsStepName(usesName),
		Run:     "true",
		Outputs: inputsOutputs,
	}

	renamed := make([]dsl.StepEntry, len(tplSpec.Steps))
	for idx, inner := range tplSpec.Steps {
		if inner.Parallel != nil {
			// Parallel block: prefix each inner step name and rewrite refs
			rp := make([]dsl.Step, len(inner.Parallel))
			for i, ps := range inner.Parallel {
				ns := ps
				ns.Name = prefixedName(usesName, ps.Name)
				ns.Run = rewriteRefs(ps.Run, usesName, innerNames)
				ns.If = rewriteRefs(ps.If, usesName, innerNames)
				ns.Env = rewriteMap(ps.Env, usesName, innerNames)
				ns.Outputs = rewriteMap(ps.Outputs, usesName, innerNames)
				if ps.Cache != nil {
					c := *ps.Cache
					c.Path = rewriteRefs(c.Path, usesName, innerNames)
					c.Key = rewriteRefs(c.Key, usesName, innerNames)
					if len(c.RestoreKeys) > 0 {
						rk := make([]string, len(c.RestoreKeys))
						for j, k := range c.RestoreKeys {
							rk[j] = rewriteRefs(k, usesName, innerNames)
						}
						c.RestoreKeys = rk
					}
					ns.Cache = &c
				}
				if ps.UploadArtifact != nil {
					ua := *ps.UploadArtifact
					ua.Name = rewriteRefs(ua.Name, usesName, innerNames)
					ua.Path = rewriteRefs(ua.Path, usesName, innerNames)
					ns.UploadArtifact = &ua
				}
				if ps.DownloadArtifact != nil {
					da := *ps.DownloadArtifact
					da.Name = rewriteRefs(da.Name, usesName, innerNames)
					da.DestDir = rewriteRefs(da.DestDir, usesName, innerNames)
					ns.DownloadArtifact = &da
				}
				if ps.Post != nil {
					p := *ps.Post
					p.Run = rewriteRefs(p.Run, usesName, innerNames)
					p.Env = rewriteMap(p.Env, usesName, innerNames)
					ns.Post = &p
				}
				if ps.Call != nil {
					c := *ps.Call
					ns.Call = &c
				}
				if ps.Uses != nil {
					return nil, fmt.Errorf("internal error: parallel step %q has unresolved nested uses; must be resolved before expandUsesStep", ps.Name)
				}
				if ps.RunsIn != nil {
					return nil, fmt.Errorf("template step %q: step-level runsIn: is no longer supported — use container: (see 2026-07-08 job isolation)", ps.Name)
				}
				if scopeMode {
					if err := checkScopeStepAllowed(ps.Name, ps.Container, ps.Approval != nil, ps.Call != nil); err != nil {
						return nil, err
					}
					ns.ScopeID = scopeID
					ns.ScopeImage = scopeImage
					ns.RunsIn = nil
				} else if ns.Container == "" {
					ns.Container = outerContainer
				}
				rp[i] = ns
			}
			renamed[idx] = dsl.StepEntry{Parallel: rp}
		} else {
			// Concrete step
			ns := dsl.StepEntry{
				Name:            prefixedName(usesName, inner.Name),
				Run:             rewriteRefs(inner.Run, usesName, innerNames),
				If:              rewriteRefs(inner.If, usesName, innerNames),
				Env:             rewriteMap(inner.Env, usesName, innerNames),
				Outputs:         rewriteMap(inner.Outputs, usesName, innerNames),
				ContinueOnError: inner.ContinueOnError,
				Container:       inner.Container,
				TimeoutMinutes:  inner.TimeoutMinutes,
				// The step's own shell: survives inlining as-is; a
				// template-level tplSpec.Shell is stamped onto steps
				// lacking one below, after all steps are renamed.
				Shell: inner.Shell,
			}
			if inner.RunsIn != nil {
				return nil, fmt.Errorf("template step %q: step-level runsIn: is no longer supported — use container: (see 2026-07-08 job isolation)", inner.Name)
			}
			if scopeMode {
				if err := checkScopeStepAllowed(inner.Name, inner.Container, inner.Approval != nil, inner.Call != nil); err != nil {
					return nil, err
				}
				ns.ScopeID = scopeID
				ns.ScopeImage = scopeImage
				ns.RunsIn = nil
			} else if ns.Container == "" {
				ns.Container = outerContainer
			}
			if inner.Cache != nil {
				c := *inner.Cache
				c.Path = rewriteRefs(c.Path, usesName, innerNames)
				c.Key = rewriteRefs(c.Key, usesName, innerNames)
				if len(c.RestoreKeys) > 0 {
					rk := make([]string, len(c.RestoreKeys))
					for i, k := range c.RestoreKeys {
						rk[i] = rewriteRefs(k, usesName, innerNames)
					}
					c.RestoreKeys = rk
				}
				ns.Cache = &c
			}
			if inner.UploadArtifact != nil {
				ua := *inner.UploadArtifact
				ua.Name = rewriteRefs(ua.Name, usesName, innerNames)
				ua.Path = rewriteRefs(ua.Path, usesName, innerNames)
				ns.UploadArtifact = &ua
			}
			if inner.DownloadArtifact != nil {
				da := *inner.DownloadArtifact
				da.Name = rewriteRefs(da.Name, usesName, innerNames)
				da.DestDir = rewriteRefs(da.DestDir, usesName, innerNames)
				ns.DownloadArtifact = &da
			}
			if inner.Post != nil {
				p := *inner.Post
				p.Run = rewriteRefs(p.Run, usesName, innerNames)
				p.Env = rewriteMap(p.Env, usesName, innerNames)
				ns.Post = &p
			}
			if inner.Call != nil {
				c := *inner.Call
				ns.Call = &c // with: values intentionally not rewritten in v1
			}
			if inner.Uses != nil {
				return nil, fmt.Errorf("internal error: step %q has unresolved nested uses; must be resolved before expandUsesStep", inner.Name)
			}
			renamed[idx] = ns
		}
	}

	// A template-level spec.shell is stamped onto every inlined step (and
	// parallel sub-step) that declares no shell: of its own. A step's own
	// shell: (already carried onto ns/rp above by copy or explicit field)
	// always wins — the template author declared it because the script
	// needs it, and the caller cannot override either value (caller-level
	// spec.shell resolution happens later, at claim build time, and only
	// fills steps still nil after this stamping — see
	// internal/controller/api_agent.go's resolveShell).
	if len(tplSpec.Shell) > 0 {
		for i := range renamed {
			if renamed[i].Parallel != nil {
				for j := range renamed[i].Parallel {
					if len(renamed[i].Parallel[j].Shell) == 0 {
						renamed[i].Parallel[j].Shell = tplSpec.Shell
					}
				}
			} else if len(renamed[i].Shell) == 0 {
				renamed[i].Shell = tplSpec.Shell
			}
		}
	}

	outputsMap := map[string]string{}
	for _, decl := range tplSpec.Params.Outputs {
		var sourceStep string
		for _, inner := range tplSpec.Steps {
			if inner.Parallel == nil {
				if _, ok := inner.Outputs[decl.Name]; ok {
					sourceStep = inner.Name
				}
			}
		}
		if sourceStep == "" {
			continue
		}
		outputsMap[decl.Name] = fmt.Sprintf("{{ .Steps.%s.Outputs.%s }}", prefixedName(usesName, sourceStep), decl.Name)
	}
	captureStep := dsl.StepEntry{
		Name:    usesName,
		Run:     "true",
		Outputs: outputsMap,
	}

	result := make([]dsl.StepEntry, 0, len(renamed)+2)
	result = append(result, inputsStep)
	result = append(result, renamed...)
	result = append(result, captureStep)
	return result, nil
}
