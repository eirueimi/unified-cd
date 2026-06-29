package dsl

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const SupportedAPIVersion = "unified-cd/v1"

var orLockNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func Parse(r io.Reader) (*Job, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	// Check for removed fields before struct decode so users get a clear error.
	if err := checkForbiddenJobFields(data); err != nil {
		return nil, err
	}
	var job Job
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&job); err != nil {
		return nil, err
	}
	if err := job.Validate(); err != nil {
		return nil, err
	}
	return &job, nil
}

func checkForbiddenJobFields(data []byte) error {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil // let the typed unmarshal report the error
	}
	spec, _ := raw["spec"].(map[string]any)
	if spec == nil {
		return nil
	}
	if _, ok := spec["failFast"]; ok {
		return fmt.Errorf("spec.failFast is no longer supported: remove it (all started steps run to completion)")
	}
	steps, _ := spec["steps"].([]any)
	if err := checkNeedsInEntries(steps, "spec.steps"); err != nil {
		return err
	}
	finally, _ := spec["finally"].([]any)
	if err := checkNeedsInEntries(finally, "spec.finally"); err != nil {
		return err
	}
	return nil
}

// checkNeedsInEntries scans a raw list of step entries (from spec.steps or
// spec.finally) and returns an error if any entry — or any inner parallel
// sub-entry — contains a forbidden needs: key.
func checkNeedsInEntries(entries []any, prefix string) error {
	for i, s := range entries {
		sm, _ := s.(map[string]any)
		if sm == nil {
			continue
		}
		if _, ok := sm["needs"]; ok {
			return fmt.Errorf("%s[%d]: needs: is no longer supported — use parallel: blocks for concurrent execution", prefix, i)
		}
		// Also check inside parallel blocks
		parallel, _ := sm["parallel"].([]any)
		for j, ps := range parallel {
			pm, _ := ps.(map[string]any)
			if pm == nil {
				continue
			}
			if _, ok := pm["needs"]; ok {
				return fmt.Errorf("%s[%d].parallel[%d]: needs: is no longer supported", prefix, i, j)
			}
		}
	}
	return nil
}

func (j *Job) Validate() error {
	if j.APIVersion != SupportedAPIVersion {
		return fmt.Errorf("unsupported apiVersion %q (want %q)", j.APIVersion, SupportedAPIVersion)
	}
	if j.Kind != "Job" {
		return fmt.Errorf("unsupported kind %q (want \"Job\")", j.Kind)
	}
	if j.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if err := ValidateName(j.Metadata.Name); err != nil {
		return fmt.Errorf("metadata.name %w", err)
	}
	if len(j.Spec.Steps) == 0 {
		return fmt.Errorf("spec.steps must contain at least one step")
	}

	// Collect step names for duplicate detection across steps and finally.
	nameSet := map[string]bool{}
	if err := validateStepEntries(j.Spec.Steps, "spec.steps", nameSet, true); err != nil {
		return err
	}
	if err := validateStepEntries(j.Spec.Finally, "spec.finally", nameSet, false); err != nil {
		return err
	}

	for i, p := range j.Spec.Params.Inputs {
		if p.Name == "" {
			return fmt.Errorf("spec.params.inputs[%d].name is required", i)
		}
		validTypes := map[string]bool{"string": true, "bool": true, "int": true, "array": true}
		if !validTypes[p.Type] {
			return fmt.Errorf("spec.params.inputs[%d].type %q is invalid (want string|bool|int|array)", i, p.Type)
		}
	}
	for i, o := range j.Spec.Params.Outputs {
		if o.Name == "" {
			return fmt.Errorf("spec.params.outputs[%d].name is required", i)
		}
		if o.Type == "" {
			return fmt.Errorf("spec.params.outputs[%d].type is required", i)
		}
	}
	if j.Spec.Concurrency != nil {
		for i, nl := range j.Spec.Concurrency.Semaphores {
			if nl.Pool == "" {
				return fmt.Errorf("spec.concurrency.semaphores[%d].pool is required", i)
			}
			if nl.Capacity <= 0 {
				return fmt.Errorf("spec.concurrency.semaphores[%d].capacity must be > 0", i)
			}
		}
		seenOrLockNames := map[string]bool{}
		for i, ol := range j.Spec.Concurrency.OrLocks {
			if ol.Name == "" {
				return fmt.Errorf("spec.concurrency.orLocks[%d].name is required", i)
			}
			if !orLockNameRe.MatchString(ol.Name) {
				return fmt.Errorf("spec.concurrency.orLocks[%d].name %q must match %s", i, ol.Name, orLockNameRe.String())
			}
			if seenOrLockNames[ol.Name] {
				return fmt.Errorf("spec.concurrency.orLocks[%d].name %q is duplicated", i, ol.Name)
			}
			seenOrLockNames[ol.Name] = true
			hasLiteral := len(ol.In.Literal) > 0
			hasExpr := ol.In.Expr != ""
			if !hasLiteral && !hasExpr {
				return fmt.Errorf("spec.concurrency.orLocks[%d].in is required", i)
			}
			if hasLiteral && hasExpr {
				return fmt.Errorf("spec.concurrency.orLocks[%d].in: cannot set both a list and an expression", i)
			}
		}
	}
	return nil
}

// validateStepEntries validates a list of StepEntry (steps or finally),
// accumulating step names into nameSet for duplicate detection across the
// whole job. pathPrefix is "spec.steps" or "spec.finally".
// allowDeferredHooks controls whether cache: and post: are permitted; pass
// false for finally entries because the agent drains postHooks/hookStack
// BEFORE running finally, so deferred hooks registered there never execute.
func validateStepEntries(entries []StepEntry, pathPrefix string, nameSet map[string]bool, allowDeferredHooks bool) error {
	for i, entry := range entries {
		if len(entry.Parallel) > 0 {
			if entry.Name != "" || entry.Run != "" || entry.Call != nil || entry.Uses != nil {
				return fmt.Errorf("%s[%d]: parallel: block must not have name, run, call, or uses fields", pathPrefix, i)
			}
			for j2, st := range entry.Parallel {
				subPath := fmt.Sprintf("%s[%d].parallel[%d]", pathPrefix, i, j2)
				if !allowDeferredHooks {
					if st.Cache != nil {
						return fmt.Errorf("%s: cache: is not supported in finally steps", subPath)
					}
					if st.Post != nil {
						return fmt.Errorf("%s: post: is not supported in finally steps", subPath)
					}
				}
				if err := validateStepFull(st.Name, st.Run, st.Call, st.Uses, st.Cache, st.Foreach, subPath, nameSet); err != nil {
					return err
				}
				if err := validateCacheStep(st.Name, st.Cache); err != nil {
					return err
				}
				if err := validateUsesStep(st.Name, st.Uses, st.Call); err != nil {
					return err
				}
				if st.Post != nil && st.Post.Run == "" {
					return fmt.Errorf("step %q: post.run is required when post is specified", st.Name)
				}
			}
		} else {
			if entry.Name == "" {
				return fmt.Errorf("%s[%d]: name is required (or use parallel: for a parallel block)", pathPrefix, i)
			}
			entryPath := fmt.Sprintf("%s[%d]", pathPrefix, i)
			if !allowDeferredHooks {
				if entry.Cache != nil {
					return fmt.Errorf("%s: cache: is not supported in finally steps", entryPath)
				}
				if entry.Post != nil {
					return fmt.Errorf("%s: post: is not supported in finally steps", entryPath)
				}
			}
			if err := validateStepFull(entry.Name, entry.Run, entry.Call, entry.Uses, entry.Cache, entry.Foreach, entryPath, nameSet); err != nil {
				return err
			}
			if err := validateCacheStep(entry.Name, entry.Cache); err != nil {
				return err
			}
			if err := validateUsesStep(entry.Name, entry.Uses, entry.Call); err != nil {
				return err
			}
			if entry.Post != nil && entry.Post.Run == "" {
				return fmt.Errorf("step %q: post.run is required when post is specified", entry.Name)
			}
		}
	}
	return nil
}

func validateStepFull(name, run string, call *CallStep, uses *UsesStep, cache *CacheStep, foreach *ForeachDef, path string, nameSet map[string]bool) error {
	if nameSet[name] {
		return fmt.Errorf("%s: duplicate step name %q", path, name)
	}
	nameSet[name] = true

	actionCount := 0
	if run != "" {
		actionCount++
	}
	if call != nil {
		actionCount++
	}
	if cache != nil {
		actionCount++
	}
	if uses != nil {
		actionCount++
	}
	if actionCount == 0 {
		return fmt.Errorf("%s (%s): one of run, call, or uses is required", path, name)
	}
	if actionCount > 1 {
		return fmt.Errorf("%s (%s): only one of run, call, cache, uses may be specified", path, name)
	}
	if call != nil && call.Job == "" {
		return fmt.Errorf("%s (%s): call.job is required", path, name)
	}
	if foreach != nil {
		if foreach.Key == "" {
			return fmt.Errorf("%s (%s): foreach.key is required", path, name)
		}
		if len(foreach.Source.Literal) == 0 && foreach.Source.Expr == "" {
			return fmt.Errorf("%s (%s): foreach.in must be a non-empty list or expression", path, name)
		}
	}
	return nil
}

func validateCacheStep(name string, cache *CacheStep) error {
	if cache == nil {
		return nil
	}
	if cache.Path == "" {
		return fmt.Errorf("step %q: cache.path is required", name)
	}
	if cache.Key == "" {
		return fmt.Errorf("step %q: cache.key is required", name)
	}
	if cache.TTLDays < 0 {
		return fmt.Errorf("step %q: cache.ttlDays must be non-negative", name)
	}
	return nil
}

func validateUsesStep(name string, uses *UsesStep, call *CallStep) error {
	if call != nil && strings.HasPrefix(call.Job, "git://") {
		return fmt.Errorf("step %q: call.job no longer supports git:// URIs; use \"uses\" instead", name)
	}
	if uses == nil {
		return nil
	}
	if uses.Job == "" {
		return fmt.Errorf("step %q: uses.job is required", name)
	}
	if !strings.HasPrefix(uses.Job, "git://") {
		return fmt.Errorf("step %q: uses.job must be a git:// URI (e.g. git://host/org/repo/path@ref)", name)
	}
	if !strings.Contains(uses.Job, "@") {
		return fmt.Errorf("step %q: git URI must contain @ref (e.g. git://host/org/repo/path@v1)", name)
	}
	if strings.Contains(uses.Job, "..") {
		return fmt.Errorf("step %q: git URI must not contain ..", name)
	}
	return nil
}
