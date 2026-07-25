package dsl

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"
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
	if hasUnsupportedSecretIndex(out) {
		return out, fmt.Errorf(
			"dynamic secret name must be resolved from a parameter before execution",
		)
	}
	return out, nil
}

// ReferencedSecretNames returns the statically named secrets in a template.
func ReferencedSecretNames(tpl string) ([]string, error) {
	searchable := templateWithoutCommentActions(tpl)
	if hasUnsupportedSecretIndex(tpl) {
		return nil, fmt.Errorf(
			"dynamic secret name must be resolved from a parameter before execution",
		)
	}

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

type secretTemplateValue uint8

const (
	secretTemplateValueUnknown secretTemplateValue = iota
	secretTemplateValueRoot
	secretTemplateValueMap
)

type secretTemplateAnalysisKey struct {
	name string
	dot  secretTemplateValue
}

type secretTemplateAnalyzer struct {
	trees       map[string]*parse.Tree
	active      map[secretTemplateAnalysisKey]bool
	unsupported bool
}

func hasUnsupportedSecretIndex(tpl string) bool {
	parsed, err := template.New("").Funcs(funcMap).Option("missingkey=zero").Parse(normalizeSecretsRefs(tpl))
	if err != nil {
		// Preserve the existing focused behavior for templates that the runtime
		// parser will reject independently.
		return hasSecretPipelineIndex(tpl)
	}

	trees := make(map[string]*parse.Tree)
	for _, defined := range parsed.Templates() {
		if defined.Tree != nil {
			trees[defined.Name()] = defined.Tree
		}
	}
	analyzer := secretTemplateAnalyzer{
		trees:  trees,
		active: make(map[secretTemplateAnalysisKey]bool),
	}
	for name := range trees {
		analyzer.analyzeTemplate(name, secretTemplateValueRoot)
		if analyzer.unsupported {
			return true
		}
	}
	return false
}

func (a *secretTemplateAnalyzer) analyzeTemplate(name string, dot secretTemplateValue) {
	if a.unsupported {
		return
	}
	key := secretTemplateAnalysisKey{name: name, dot: dot}
	if a.active[key] {
		return
	}
	tree := a.trees[name]
	if tree == nil || tree.Root == nil {
		return
	}
	a.active[key] = true
	defer delete(a.active, key)

	vars := map[string]secretTemplateValue{"$": dot}
	a.analyzeList(tree.Root, dot, vars)
}

func (a *secretTemplateAnalyzer) analyzeList(list *parse.ListNode, dot secretTemplateValue, vars map[string]secretTemplateValue) {
	if list == nil || a.unsupported {
		return
	}
	for _, node := range list.Nodes {
		switch node := node.(type) {
		case *parse.ActionNode:
			value := a.analyzePipe(node.Pipe, dot, vars)
			assignTemplateVariables(vars, node.Pipe, value)
		case *parse.IfNode:
			a.analyzeBranch(&node.BranchNode, dot, vars, dot, false)
		case *parse.WithNode:
			value := a.analyzePipe(node.Pipe, dot, vars)
			a.analyzeBranch(&node.BranchNode, dot, vars, value, true)
		case *parse.RangeNode:
			a.analyzeRange(node, dot, vars)
		case *parse.TemplateNode:
			templateDot := a.analyzePipe(node.Pipe, dot, vars)
			a.analyzeTemplate(node.Name, templateDot)
		}
		if a.unsupported {
			return
		}
	}
}

func (a *secretTemplateAnalyzer) analyzeBranch(
	branch *parse.BranchNode,
	dot secretTemplateValue,
	vars map[string]secretTemplateValue,
	listDot secretTemplateValue,
	pipeAlreadyAnalyzed bool,
) {
	value := listDot
	if !pipeAlreadyAnalyzed {
		value = a.analyzePipe(branch.Pipe, dot, vars)
	}
	branchVars := cloneSecretTemplateVars(vars)
	assignTemplateVariables(branchVars, branch.Pipe, value)
	a.analyzeList(branch.List, listDot, branchVars)

	elseVars := cloneSecretTemplateVars(vars)
	assignTemplateVariables(elseVars, branch.Pipe, value)
	a.analyzeList(branch.ElseList, dot, elseVars)
	mergeSecretTemplateAssignments(vars, branchVars, elseVars)
}

func (a *secretTemplateAnalyzer) analyzeRange(node *parse.RangeNode, dot secretTemplateValue, vars map[string]secretTemplateValue) {
	a.analyzePipe(node.Pipe, dot, vars)
	rangeVars := cloneSecretTemplateVars(vars)
	for _, variable := range node.Pipe.Decl {
		rangeVars[variable.Ident[0]] = secretTemplateValueUnknown
	}
	a.analyzeList(node.List, secretTemplateValueUnknown, rangeVars)

	elseVars := cloneSecretTemplateVars(vars)
	a.analyzeList(node.ElseList, dot, elseVars)
	mergeSecretTemplateAssignments(vars, rangeVars, elseVars)
}

func (a *secretTemplateAnalyzer) analyzePipe(
	pipe *parse.PipeNode,
	dot secretTemplateValue,
	vars map[string]secretTemplateValue,
) secretTemplateValue {
	if pipe == nil {
		return dot
	}
	for _, command := range pipe.Cmds {
		a.analyzeCommand(command, dot, vars)
	}
	if len(pipe.Cmds) != 1 || len(pipe.Cmds[0].Args) != 1 {
		return secretTemplateValueUnknown
	}
	return secretTemplateNodeValue(pipe.Cmds[0].Args[0], dot, vars)
}

func (a *secretTemplateAnalyzer) analyzeCommand(
	command *parse.CommandNode,
	dot secretTemplateValue,
	vars map[string]secretTemplateValue,
) {
	for _, arg := range command.Args {
		if nested, ok := arg.(*parse.PipeNode); ok {
			a.analyzePipe(nested, dot, vars)
		}
	}
	if len(command.Args) == 0 {
		return
	}
	identifier, ok := command.Args[0].(*parse.IdentifierNode)
	if !ok || identifier.Ident != "index" || len(command.Args) < 2 {
		return
	}
	if secretTemplateNodeValue(command.Args[1], dot, vars) != secretTemplateValueMap {
		return
	}
	if len(command.Args) == 3 && isCanonicalSecretMapNode(command.Args[1]) {
		if _, ok := command.Args[2].(*parse.StringNode); ok {
			return
		}
	}
	a.unsupported = true
}

func secretTemplateNodeValue(
	node parse.Node,
	dot secretTemplateValue,
	vars map[string]secretTemplateValue,
) secretTemplateValue {
	switch node := node.(type) {
	case *parse.DotNode:
		return dot
	case *parse.FieldNode:
		if dot == secretTemplateValueRoot && len(node.Ident) == 1 && node.Ident[0] == "Secrets" {
			return secretTemplateValueMap
		}
	case *parse.VariableNode:
		value := vars[node.Ident[0]]
		if len(node.Ident) == 1 {
			return value
		}
		if value == secretTemplateValueRoot && len(node.Ident) == 2 && node.Ident[1] == "Secrets" {
			return secretTemplateValueMap
		}
	case *parse.PipeNode:
		if len(node.Cmds) == 1 && len(node.Cmds[0].Args) == 1 {
			return secretTemplateNodeValue(node.Cmds[0].Args[0], dot, vars)
		}
	case *parse.ChainNode:
		value := secretTemplateNodeValue(node.Node, dot, vars)
		if value == secretTemplateValueRoot && len(node.Field) == 1 && node.Field[0] == "Secrets" {
			return secretTemplateValueMap
		}
	}
	return secretTemplateValueUnknown
}

func isCanonicalSecretMapNode(node parse.Node) bool {
	field, ok := node.(*parse.FieldNode)
	return ok && len(field.Ident) == 1 && field.Ident[0] == "Secrets"
}

func assignTemplateVariables(vars map[string]secretTemplateValue, pipe *parse.PipeNode, value secretTemplateValue) {
	if pipe == nil {
		return
	}
	for _, variable := range pipe.Decl {
		vars[variable.Ident[0]] = value
	}
}

func cloneSecretTemplateVars(vars map[string]secretTemplateValue) map[string]secretTemplateValue {
	cloned := make(map[string]secretTemplateValue, len(vars))
	for name, value := range vars {
		cloned[name] = value
	}
	return cloned
}

func mergeSecretTemplateAssignments(
	vars map[string]secretTemplateValue,
	branches ...map[string]secretTemplateValue,
) {
	for name, original := range vars {
		merged := original
		for _, branch := range branches {
			if branch[name] == secretTemplateValueMap {
				merged = secretTemplateValueMap
				break
			}
			if branch[name] != merged {
				merged = secretTemplateValueUnknown
			}
		}
		vars[name] = merged
	}
}

func hasSecretPipelineIndex(tpl string) bool {
	for offset := 0; offset < len(tpl); {
		start := strings.Index(tpl[offset:], "{{")
		if start < 0 {
			return false
		}
		start += offset + len("{{")
		end, ok := templateActionEnd(tpl, start)
		if !ok {
			return false
		}
		action := tpl[start:end]
		if !isTemplateCommentAction(action) && actionHasSecretPipelineIndex(action) {
			return true
		}
		offset = end + len("}}")
	}
	return false
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

func actionHasSecretPipelineIndex(action string) bool {
	var quote byte
	for i := 0; i < len(action); i++ {
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
		case '|':
			indexStart := skipTemplateWhitespace(action, i+1)
			if !strings.HasPrefix(action[indexStart:], "index") {
				continue
			}
			operandStart := indexStart + len("index")
			if operandStart == len(action) || !isTemplateWhitespace(action[operandStart]) {
				continue
			}
			secretStart := skipTemplateWhitespace(action, operandStart)
			if !strings.HasPrefix(action[secretStart:], ".Secrets") {
				continue
			}
			secretEnd := secretStart + len(".Secrets")
			if secretEnd == len(action) || isTemplateWhitespace(action[secretEnd]) {
				return true
			}
		}
	}
	return false
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
