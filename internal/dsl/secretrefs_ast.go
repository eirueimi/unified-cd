package dsl

import (
	"errors"
	"fmt"
	"text/template"
	"text/template/parse"
)

var errDynamicSecretName = errors.New(
	"dynamic secret name must be resolved from a parameter before execution",
)

func validateSecretReferences(tpl string) error {
	_, err := referencedSecretNamesFromAST(tpl)
	return err
}

func referencedSecretNamesFromAST(tpl string) ([]string, error) {
	parsed, err := template.New("").
		Funcs(funcMap).
		Option("missingkey=zero").
		Parse(normalizeSecretsRefs(tpl))
	if err != nil {
		return nil, fmt.Errorf("parse secret references: %w", err)
	}
	var names []string
	for _, defined := range parsed.Templates() {
		if defined.Tree == nil || defined.Tree.Root == nil {
			continue
		}
		if err := collectSecretReferenceNames(defined.Tree.Root, &names); err != nil {
			return nil, err
		}
	}
	return names, nil
}

func collectSecretReferenceNames(node parse.Node, names *[]string) error {
	if node == nil {
		return nil
	}
	switch node := node.(type) {
	case *parse.ListNode:
		if node == nil {
			return nil
		}
		for _, child := range node.Nodes {
			if err := collectSecretReferenceNames(child, names); err != nil {
				return err
			}
		}
	case *parse.ActionNode:
		return collectSecretReferenceNames(node.Pipe, names)
	case *parse.IfNode:
		return collectSecretReferenceNamesFromBranch(&node.BranchNode, names)
	case *parse.WithNode:
		return collectSecretReferenceNamesFromBranch(&node.BranchNode, names)
	case *parse.RangeNode:
		return collectSecretReferenceNamesFromBranch(&node.BranchNode, names)
	case *parse.TemplateNode:
		return collectSecretReferenceNames(node.Pipe, names)
	case *parse.PipeNode:
		if node == nil {
			return nil
		}
		for _, command := range node.Cmds {
			if err := collectSecretReferenceNamesFromCommand(command, names); err != nil {
				return err
			}
		}
	case *parse.CommandNode:
		return collectSecretReferenceNamesFromCommand(node, names)
	case *parse.FieldNode:
		if isAllowedDirectSecretValue(node) {
			name := node.Ident[1]
			if err := ValidateSecretName(name); err != nil {
				return fmt.Errorf("secret name %q %w", name, err)
			}
			*names = append(*names, name)
			return nil
		}
		if containsReservedSecrets(node.Ident) {
			return errDynamicSecretName
		}
	case *parse.VariableNode:
		if containsReservedSecrets(node.Ident) {
			return errDynamicSecretName
		}
	case *parse.ChainNode:
		if containsReservedSecrets(node.Field) {
			return errDynamicSecretName
		}
		return collectSecretReferenceNames(node.Node, names)
	}
	return nil
}

func collectSecretReferenceNamesFromBranch(branch *parse.BranchNode, names *[]string) error {
	if err := collectSecretReferenceNames(branch.Pipe, names); err != nil {
		return err
	}
	if err := collectSecretReferenceNames(branch.List, names); err != nil {
		return err
	}
	return collectSecretReferenceNames(branch.ElseList, names)
}

func collectSecretReferenceNamesFromCommand(command *parse.CommandNode, names *[]string) error {
	if name, ok := canonicalSecretIndexName(command); ok {
		if name == "" {
			return nil
		}
		if err := ValidateSecretName(name); err != nil {
			return fmt.Errorf("secret name %q %w", name, err)
		}
		*names = append(*names, name)
		return nil
	}
	for _, argument := range command.Args {
		if err := collectSecretReferenceNames(argument, names); err != nil {
			return err
		}
	}
	return nil
}

func canonicalSecretIndexName(command *parse.CommandNode) (string, bool) {
	if len(command.Args) != 3 {
		return "", false
	}
	identifier, ok := command.Args[0].(*parse.IdentifierNode)
	if !ok || identifier.Ident != "index" {
		return "", false
	}
	receiver, ok := command.Args[1].(*parse.FieldNode)
	if !ok || len(receiver.Ident) != 1 || receiver.Ident[0] != "Secrets" {
		return "", false
	}
	key, ok := command.Args[2].(*parse.StringNode)
	if !ok {
		return "", false
	}
	return key.Text, true
}

func isAllowedDirectSecretValue(field *parse.FieldNode) bool {
	return len(field.Ident) == 2 && field.Ident[0] == "Secrets"
}

func containsReservedSecrets(fields []string) bool {
	for _, field := range fields {
		if field == "Secrets" {
			return true
		}
	}
	return false
}
