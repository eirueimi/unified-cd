package dsl

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	secretIndexRe = regexp.MustCompile(
		`index[ \t\r\n]+\.Secrets[ \t\r\n]+("(?:\\.|[^"\\])*"|[^\s}]+)`,
	)
	secretPipelineIndexRe = regexp.MustCompile(
		`(?s){{-?[ \t\r\n]*[^}]*\|[ \t\r\n]*index[ \t\r\n]+\.Secrets(?:[ \t\r\n]|}})`,
	)
	secretParamOperandRe = regexp.MustCompile(
		`^\.Params\.([A-Za-z_][A-Za-z0-9_]*)$`,
	)
	directSecretRefRe = regexp.MustCompile(
		`(?:secrets|\.Secrets)\.([A-Za-z_][A-Za-z0-9_-]*)`,
	)
)

// ResolveSecretNameParams replaces parameter operands in secret index
// expressions with validated string literals before a template is executed.
func ResolveSecretNameParams(tpl string, params map[string]string) (string, error) {
	var resolveErr error
	out := secretIndexRe.ReplaceAllStringFunc(tpl, func(match string) string {
		if resolveErr != nil {
			return match
		}
		sub := secretIndexRe.FindStringSubmatch(match)
		operand := sub[1]
		if strings.HasPrefix(operand, `"`) {
			name, err := strconv.Unquote(operand)
			if err != nil {
				resolveErr = fmt.Errorf("invalid secret name literal: %w", err)
				return match
			}
			if name != "" {
				if err := ValidateSecretName(name); err != nil {
					resolveErr = fmt.Errorf("secret name %q %w", name, err)
				}
			}
			return match
		}

		param := secretParamOperandRe.FindStringSubmatch(operand)
		if len(param) != 2 {
			resolveErr = fmt.Errorf(
				"dynamic secret name must be resolved from a parameter before execution",
			)
			return match
		}
		paramName := param[1]
		name := params[paramName]
		if name != "" {
			if strings.Contains(name, "{{") || strings.Contains(name, "}}") {
				resolveErr = fmt.Errorf(
					"secret name parameter %q must be a literal secret name",
					paramName,
				)
				return match
			}
			if err := ValidateSecretName(name); err != nil {
				resolveErr = fmt.Errorf(
					"secret name parameter %q %w",
					paramName,
					err,
				)
				return match
			}
		}
		prefix := strings.TrimSuffix(match, operand)
		return prefix + strconv.Quote(name)
	})
	if resolveErr != nil {
		return out, resolveErr
	}
	if secretPipelineIndexRe.MatchString(tpl) {
		return out, fmt.Errorf(
			"dynamic secret name must be resolved from a parameter before execution",
		)
	}
	return out, nil
}

// ReferencedSecretNames returns the statically named secrets in a template.
func ReferencedSecretNames(tpl string) ([]string, error) {
	if secretPipelineIndexRe.MatchString(tpl) {
		return nil, fmt.Errorf(
			"dynamic secret name must be resolved from a parameter before execution",
		)
	}

	var names []string
	for _, match := range directSecretRefRe.FindAllStringSubmatch(tpl, -1) {
		name := match[1]
		if err := ValidateSecretName(name); err != nil {
			return nil, fmt.Errorf("secret name %q %w", name, err)
		}
		names = append(names, name)
	}

	for _, match := range secretIndexRe.FindAllStringSubmatch(tpl, -1) {
		operand := match[1]
		if !strings.HasPrefix(operand, `"`) {
			return nil, fmt.Errorf(
				"dynamic secret name must be resolved from a parameter before execution",
			)
		}
		name, err := strconv.Unquote(operand)
		if err != nil {
			return nil, fmt.Errorf("invalid secret name literal: %w", err)
		}
		if name == "" {
			continue
		}
		if err := ValidateSecretName(name); err != nil {
			return nil, fmt.Errorf("secret name %q %w", name, err)
		}
		names = append(names, name)
	}
	return names, nil
}

// ResolveSecretNameParamsInSpec resolves parameter-selected secret names in
// executable fields throughout a job specification.
func ResolveSecretNameParamsInSpec(spec *Spec, params map[string]string) error {
	if err := resolveSecretNameParamsInEntries(spec.Steps, params); err != nil {
		return err
	}
	return resolveSecretNameParamsInEntries(spec.Finally, params)
}

func resolveSecretNameParamsInEntries(entries []StepEntry, params map[string]string) error {
	for i := range entries {
		entry := &entries[i]
		if len(entry.Parallel) > 0 {
			for j := range entry.Parallel {
				step := &entry.Parallel[j]
				if err := resolveSecretNameParamsInStep(step.Name, &step.Run, step.Env, params); err != nil {
					return err
				}
			}
			continue
		}
		if err := resolveSecretNameParamsInStep(entry.Name, &entry.Run, entry.Env, params); err != nil {
			return err
		}
	}
	return nil
}

func resolveSecretNameParamsInStep(name string, run *string, env map[string]string, params map[string]string) error {
	resolvedRun, err := ResolveSecretNameParams(*run, params)
	if err != nil {
		return fmt.Errorf("step %q run: %w", name, err)
	}
	*run = resolvedRun
	for key, value := range env {
		resolved, err := ResolveSecretNameParams(value, params)
		if err != nil {
			return fmt.Errorf("step %q env %q: %w", name, key, err)
		}
		env[key] = resolved
	}
	return nil
}
