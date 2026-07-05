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

func prefixedName(usesName, innerName string) string {
	return usesName + usesPrefixSep + innerName
}

func inputsStepName(usesName string) string {
	return usesName + usesPrefixSep + "inputs"
}

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
//	<usesName>__inputs              synthetic: captures `with` (evaluated in the
//	                                 parent's runtime context at this position)
//	<usesName>__<innerStep...>       tplSpec's own steps, renamed + reference-rewritten
//	<usesName>                       synthetic: captures tplSpec's declared
//	                                 spec.params.outputs (always present, even if empty)
//
// Order is sequential (array position); Needs is no longer used.
// with is the already-stringified `uses.with` map (via UsesStep.WithAsStrings).
// tplSpec must have at least one step (the caller is responsible for having already
// validated the fetched job, e.g. via dsl.Job.Validate()).
// outerRunsIn is the RunsIn declared on the outer `uses` step (may be nil); each
// inlined template step keeps its own RunsIn if set, otherwise inherits outerRunsIn.
func expandUsesStep(usesName string, with map[string]string, tplSpec dsl.Spec, outerRunsIn *dsl.RunsIn) ([]dsl.StepEntry, error) {
	if len(tplSpec.Steps) == 0 {
		return nil, fmt.Errorf("template job has no steps")
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

	inputsOutputs := make(map[string]string, len(with))
	for k, v := range with {
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
				ns.RunsIn = ps.RunsIn
				if ns.RunsIn == nil {
					ns.RunsIn = outerRunsIn
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
			}
			ns.RunsIn = inner.RunsIn
			if ns.RunsIn == nil {
				ns.RunsIn = outerRunsIn
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
