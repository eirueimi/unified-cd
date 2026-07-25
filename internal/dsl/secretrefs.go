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
	out := replaceSecretIndexMatches(tpl, func(match string) string {
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
			resolveErr = errDynamicSecretName
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
	if err := validateSecretReferences(out); err != nil {
		return out, err
	}
	return out, nil
}

// ReferencedSecretNames returns the statically named secrets in a template.
func ReferencedSecretNames(tpl string) ([]string, error) {
	if err := validateSecretReferences(tpl); err != nil {
		return nil, err
	}
	searchable := templateWithoutCommentActions(tpl)

	var names []string
	for _, match := range directSecretRefRe.FindAllStringSubmatch(searchable, -1) {
		name := match[1]
		if err := ValidateSecretName(name); err != nil {
			return nil, fmt.Errorf("secret name %q %w", name, err)
		}
		names = append(names, name)
	}

	for _, match := range secretIndexMatches(searchable) {
		operand := secretIndexRe.FindStringSubmatch(match)[1]
		if !strings.HasPrefix(operand, `"`) {
			return nil, errDynamicSecretName
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

func replaceSecretIndexMatches(tpl string, replace func(string) string) string {
	var out strings.Builder
	offset := 0
	for offset < len(tpl) {
		start := strings.Index(tpl[offset:], "{{")
		if start < 0 {
			break
		}
		start += offset + len("{{")
		end, ok := templateActionEnd(tpl, start)
		if !ok {
			break
		}
		out.WriteString(tpl[offset:start])
		action := tpl[start:end]
		if isTemplateCommentAction(action) {
			out.WriteString(action)
		} else {
			out.WriteString(replaceSecretIndexMatchesInAction(action, replace))
		}
		offset = end
	}
	out.WriteString(tpl[offset:])
	return out.String()
}

func secretIndexMatches(tpl string) []string {
	var matches []string
	for offset := 0; offset < len(tpl); {
		start := strings.Index(tpl[offset:], "{{")
		if start < 0 {
			break
		}
		start += offset + len("{{")
		end, ok := templateActionEnd(tpl, start)
		if !ok {
			break
		}
		action := tpl[start:end]
		if isTemplateCommentAction(action) {
			offset = end + len("}}")
			continue
		}
		for _, match := range secretIndexRe.FindAllStringIndex(action, -1) {
			if !templatePositionQuoted(action, match[0]) {
				matches = append(matches, tpl[start+match[0]:start+match[1]])
			}
		}
		offset = end + len("}}")
	}
	return matches
}

func replaceSecretIndexMatchesInAction(action string, replace func(string) string) string {
	matches := secretIndexRe.FindAllStringIndex(action, -1)
	if len(matches) == 0 {
		return action
	}
	var out strings.Builder
	offset := 0
	for _, match := range matches {
		if templatePositionQuoted(action, match[0]) {
			continue
		}
		out.WriteString(action[offset:match[0]])
		out.WriteString(replace(action[match[0]:match[1]]))
		offset = match[1]
	}
	out.WriteString(action[offset:])
	return out.String()
}

func templatePositionQuoted(action string, position int) bool {
	var quote byte
	for i := 0; i < position; i++ {
		char := action[i]
		if quote != 0 {
			if quote != '`' && char == '\\' {
				i++
				continue
			}
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '"', '\'', '`':
			quote = char
		}
	}
	return quote != 0
}

func templateActionEnd(tpl string, start int) (int, bool) {
	commentStart := skipTemplateWhitespace(tpl, start)
	if commentStart < len(tpl) && tpl[commentStart] == '-' {
		commentStart = skipTemplateWhitespace(tpl, commentStart+1)
	}
	if strings.HasPrefix(tpl[commentStart:], "/*") {
		commentEnd := strings.Index(tpl[commentStart+len("/*"):], "*/")
		if commentEnd < 0 {
			return 0, false
		}
		commentEnd += commentStart + len("/*")
		close := strings.Index(tpl[commentEnd+len("*/"):], "}}")
		if close < 0 {
			return 0, false
		}
		return commentEnd + len("*/") + close, true
	}

	var quote byte
	for i := start; i < len(tpl); i++ {
		char := tpl[i]
		if quote != 0 {
			if quote != '`' && char == '\\' {
				i++
				continue
			}
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '"', '\'', '`':
			quote = char
		case '}':
			if i+1 < len(tpl) && tpl[i+1] == '}' {
				return i, true
			}
		}
	}
	return 0, false
}

func isTemplateCommentAction(action string) bool {
	start := skipTemplateWhitespace(action, 0)
	if start < len(action) && action[start] == '-' {
		start = skipTemplateWhitespace(action, start+1)
	}
	return strings.HasPrefix(action[start:], "/*")
}

func templateWithoutCommentActions(tpl string) string {
	var out strings.Builder
	offset := 0
	for offset < len(tpl) {
		open := strings.Index(tpl[offset:], "{{")
		if open < 0 {
			break
		}
		open += offset
		actionStart := open + len("{{")
		end, ok := templateActionEnd(tpl, actionStart)
		if !ok {
			break
		}
		if isTemplateCommentAction(tpl[actionStart:end]) {
			out.WriteString(tpl[offset:open])
			out.WriteString("{{}}")
		} else {
			out.WriteString(tpl[offset : end+len("}}")])
		}
		offset = end + len("}}")
	}
	out.WriteString(tpl[offset:])
	return out.String()
}

func skipTemplateWhitespace(value string, start int) int {
	for start < len(value) && isTemplateWhitespace(value[start]) {
		start++
	}
	return start
}

func isTemplateWhitespace(char byte) bool {
	return char == ' ' || char == '\t' || char == '\r' || char == '\n'
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
